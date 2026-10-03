package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

// telegramClient is a deliberately tiny wrapper over the Bot API's sendMessage.
// There is no dependency on a Telegram SDK: the whole surface used here is one
// POST, and pulling a full client library into a binary whose only other job is
// holding a LISTEN open is not worth the supply chain.
type telegramClient struct {
	token   string
	chatID  string
	baseURL string
	client  *http.Client

	// maxSendAttempts bounds the retry loop. Telegram's own guidance is to
	// retry transient 5xx/network errors with backoff and to honour 429's
	// retry_after; a login code is only useful for ten minutes, so give up
	// quickly rather than blocking the notification loop.
	maxSendAttempts int
}

type sendMessageRequest struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

// telegramError carries the fields of a Bot API error response worth acting
// on. The token is never included: Telegram echoes the bot id in some errors,
// and these end up in pod logs.
type telegramError struct {
	StatusCode   int
	Description  string
	ErrorCode    int
	RetryAfter   time.Duration
	HTTPResponse string
}

func (e *telegramError) Error() string {
	if e.ErrorCode != 0 {
		return fmt.Sprintf("telegram api error %d: %s", e.ErrorCode, e.Description)
	}
	return fmt.Sprintf("telegram http %d: %s", e.StatusCode, e.Description)
}

func (c *telegramClient) sendMessage(ctx context.Context, text string) error {
	if c.maxSendAttempts == 0 {
		c.maxSendAttempts = 3
	}

	body, err := json.Marshal(sendMessageRequest{
		ChatID:    c.chatID,
		Text:      text,
		ParseMode: "HTML",
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", c.baseURL, c.token)

	var lastErr error
	for attempt := 1; attempt <= c.maxSendAttempts; attempt++ {
		// Detach from the watcher's context so a shutdown does not abort a
		// message that is already in flight.
		reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("post sendMessage: %w", err)
		} else {
			lastErr = parseTelegramResponse(resp)
			cancel()
			if lastErr == nil {
				return nil
			}
		}

		var apiErr *telegramError
		if ok := asTelegramError(lastErr, &apiErr); ok {
			// 400 with "chat not found"/"bot was blocked" will never succeed on
			// retry; fail fast so the misconfiguration is obvious.
			if apiErr.ErrorCode == 400 || apiErr.StatusCode == 401 || apiErr.StatusCode == 403 {
				return lastErr
			}
			if apiErr.RetryAfter > 0 {
				if err := sleepCtx(ctx, apiErr.RetryAfter); err != nil {
					return err
				}
				continue
			}
		}
		if err := sleepCtx(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", c.maxSendAttempts, lastErr)
}

func parseTelegramResponse(resp *http.Response) error {
	defer resp.Body.Close()

	// The Bot API returns 200 with ok:false for some failures, so the body has
	// to be inspected even on success.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(raw, &envelope)

	if resp.StatusCode == http.StatusOK && envelope.OK {
		return nil
	}

	desc := envelope.Description
	if desc == "" {
		desc = strings.TrimSpace(string(raw))
	}
	return &telegramError{
		StatusCode:   resp.StatusCode,
		Description:  desc,
		ErrorCode:    envelope.ErrorCode,
		RetryAfter:   time.Duration(envelope.Parameters.RetryAfter) * time.Second,
		HTTPResponse: resp.Status,
	}
}

func asTelegramError(err error, target **telegramError) bool {
	for err != nil {
		if te, ok := err.(*telegramError); ok {
			*target = te
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// formatMessage renders the Telegram message. HTML parse mode is used for the
// code so it renders monospaced and visually distinct; every interpolated value
// is escaped, because the email column is user-controlled text that reaches a
// Telegram chat.
func formatMessage(p verificationPayload) string {
	var b strings.Builder
	b.WriteString("🔐 <b>Multica login code</b>\n\n")
	fmt.Fprintf(&b, "Email: %s\n", html.EscapeString(p.Email))
	fmt.Fprintf(&b, "Code: <code>%s</code>\n", html.EscapeString(p.Code))
	if ttl := humanizeTTL(p.ExpiresAt); ttl != "" {
		fmt.Fprintf(&b, "Expires: %s\n", html.EscapeString(ttl))
	}
	return b.String()
}

// humanizeTTL renders the absolute expiry plus the remaining lifetime, which
// is the part that actually decides whether to go and use the code.
func humanizeTTL(expiresAt string) string {
	if expiresAt == "" {
		return ""
	}
	ts, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return expiresAt
	}
	remaining := time.Until(ts)
	if remaining < 0 {
		return expiresAt + " (expired)"
	}
	return fmt.Sprintf("%s (%s)", ts.UTC().Format("15:04:05 UTC"), remaining.Round(time.Minute))
}
