// Command multica-code-notifier watches the multica `verification_code`
// table for new rows and forwards each login code to a Telegram chat.
//
// Multica's self-hosted login emails a six-digit code, but the self-host stack
// only sends real mail if you configure Resend or an SMTP relay. Without one it
// prints the code to the backend log, which means every login needs a
// `kubectl logs`. This closes that gap: a Postgres trigger publishes each new
// row over LISTEN/NOTIFY and the program relays it to Telegram.
//
// It talks to the database directly rather than the multica API on purpose —
// the code never has to be read out of a pod log, and nothing changes when the
// multica Deployment is rescheduled.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// defaultChannel is the Postgres notification channel the trigger publishes
// on. It is namespaced to this program so it cannot collide with anything else
// listening in the multica database.
const defaultChannel = "multica_verification_code"

// defaultChatMapFile is where the Kubernetes ConfigMap is mounted. Keeping
// routing in a file rather than an env var means a team can edit the map with
// `kubectl edit configmap` and only needs to restart the pod -- no Secret
// round-trip and no risk of the same JSON being hand-copied into two places.
const defaultChatMapFile = "/etc/multica-code-notifier/chat-map.json"

var validIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type config struct {
	// Postgres connection to the multica database.
	pgHost     string
	pgPort     string
	pgDatabase string
	pgUser     string
	pgPassword string
	pgSSLMode  string

	// Table to watch: <schema>.<table>.
	tableSchema string
	table       string
	channel     string

	telegramToken  string
	telegramAPIURL string

	// Per-address routing: lower-cased email -> Telegram chat ids. An address
	// with no entry is skipped, so a shared bot can serve a whole team without
	// one person's login code landing in somebody else's chat.
	telegramChats    map[string][]string
	telegramChatFile string

	// Tuning.
	maxBackoff  time.Duration
	sendTimeout time.Duration
}

type options struct {
	showConfig    bool
	setupOnly     bool
	sendTest      string
	sendTestEmail string
	table         string
	channel       string
}

func main() {
	var opts options
	flag.BoolVar(&opts.showConfig, "show-config", false, "print the resolved configuration (secrets redacted) and exit")
	flag.BoolVar(&opts.setupOnly, "setup-only", false, "install the NOTIFY trigger, then exit without listening")
	flag.StringVar(&opts.sendTest, "send-test", "", "send this text to the mapped chat and exit (no database involvement)")
	flag.StringVar(&opts.sendTestEmail, "send-test-email", "", "address to use with -send-test (required when the map has more than one entry)")
	flag.StringVar(&opts.table, "table", "verification_code", "table to watch (without schema)")
	flag.StringVar(&opts.channel, "channel", defaultChannel, "Postgres NOTIFY channel to listen on")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log, opts); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, opts options) error {
	cfg, err := loadConfig(opts.table, opts.channel)
	if err != nil {
		return err
	}

	if opts.showConfig {
		fmt.Printf("postgres      %s@%s:%s/%s (sslmode=%s)\n",
			cfg.pgUser, cfg.pgHost, cfg.pgPort, cfg.pgDatabase, cfg.pgSSLMode)
		fmt.Printf("watching      %s.%s\n", cfg.tableSchema, cfg.table)
		fmt.Printf("channel       %s\n", cfg.channel)
		fmt.Printf("telegram      %s (token %s)\n", cfg.telegramAPIURL, maskToken(cfg.telegramToken))
		fmt.Printf("recipients    %d address(es)\n", len(cfg.telegramChats))
		for _, email := range sortedEmails(cfg.telegramChats) {
			fmt.Printf("  %-40s %s\n", email, strings.Join(cfg.telegramChats[email], ", "))
		}
		return nil
	}

	// SIGINT/SIGTERM cancels ctx, which unwinds the WaitForNotification loop
	// and the reconnect backoff. Needed so a rollout does not wait out the
	// termination grace period.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tg := &telegramClient{
		token:   cfg.telegramToken,
		baseURL: strings.TrimRight(cfg.telegramAPIURL, "/"),
		client:  &http.Client{Timeout: cfg.sendTimeout},
	}

	// Smoke-test the Telegram credentials before touching the database, so a
	// bad token fails loudly at startup instead of silently on the first code.
	// getMe does it without sending a message: with per-address routing there is
	// no single "the" chat, and greeting a random teammate on every rollout is
	// noise.
	bot, err := tg.getMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram startup check failed: %w", err)
	}
	log.Info("telegram reachable", "bot", bot, "mapped_addresses", len(cfg.telegramChats))

	if opts.sendTest != "" {
		email, chats, err := cfg.testTarget(opts.sendTestEmail)
		if err != nil {
			return err
		}
		for _, chatID := range chats {
			if err := tg.sendMessage(ctx, chatID, opts.sendTest); err != nil {
				return fmt.Errorf("send test message to %s (chat %s): %w", email, chatID, err)
			}
			log.Info("test message sent", "email", email, "chat_id", chatID)
		}
		return nil
	}

	notifier := &notifier{cfg: cfg, log: log, tg: tg}

	if opts.setupOnly {
		conn, err := connect(ctx, cfg)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		return notifier.installTrigger(ctx, conn)
	}

	// The reconnect loop is the whole program: LISTEN is bound to one
	// connection, and the database behind multica can be restarted, rescheduled
	// or failed over at any time. Everything below returns on connection loss
	// and is retried with backoff.
	return notifier.watch(ctx)
}

// loadConfig builds the configuration from the environment, applying defaults
// so the program runs unmodified against a typical in-cluster Postgres while
// still being configurable for local development. Credentials arrive as env
// vars, normally wired from Kubernetes Secrets.
func loadConfig(table, channel string) (*config, error) {
	cfg := &config{
		// Defaults for a Postgres running inside the cluster. Override for a
		// managed database, a sidecar, or a local tunnel.
		pgHost:     env("PGHOST", "postgres"),
		pgPort:     env("PGPORT", "5432"),
		pgDatabase: env("PGDATABASE", "multica"),
		pgUser:     env("PGUSER", "multica"),
		pgPassword: os.Getenv("PGPASSWORD"),
		// The database is normally reached over a private network without a
		// verifiable certificate, so certificate verification is opt-in.
		pgSSLMode: env("PGSSLMODE", "disable"),

		tableSchema: env("PG_SCHEMA", "public"),
		table:       table,
		channel:     channel,

		telegramToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		telegramAPIURL: env("TELEGRAM_API_URL", "https://api.telegram.org"),

		// The ConfigMap is the normal source in-cluster. TELEGRAM_CHAT_MAP is
		// an inline override, which keeps local runs and `docker run` simple.
		telegramChatFile: env("TELEGRAM_CHAT_MAP_FILE", defaultChatMapFile),

		maxBackoff:  30 * time.Second,
		sendTimeout: 15 * time.Second,
	}

	// Inline JSON wins over the file; if neither is present, validate() fails
	// with a message naming both.
	if raw := os.Getenv("TELEGRAM_CHAT_MAP"); raw != "" {
		chats, err := parseChatMap(raw)
		if err != nil {
			return nil, fmt.Errorf("TELEGRAM_CHAT_MAP: %w", err)
		}
		cfg.telegramChats = chats
	} else if data, err := os.ReadFile(cfg.telegramChatFile); err == nil {
		chats, err := parseChatMap(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.telegramChatFile, err)
		}
		cfg.telegramChats = chats
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read chat map %s: %w", cfg.telegramChatFile, err)
	}

	// These get interpolated into DDL below, so they must be plain
	// identifiers -- a name containing a quote would otherwise be able to
	// change the statement. pgx.Identifier sanitisation covers the table, but
	// the channel name lives inside the function body as a string literal.
	if !validIdentifier.MatchString(cfg.channel) {
		return nil, fmt.Errorf("invalid channel %q: must match %s", cfg.channel, validIdentifier)
	}
	if !validIdentifier.MatchString(cfg.tableSchema) {
		return nil, fmt.Errorf("invalid schema %q: must match %s", cfg.tableSchema, validIdentifier)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *config) validate() error {
	var missing []string
	if c.pgPassword == "" {
		missing = append(missing, "PGPASSWORD")
	}
	if c.telegramToken == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if len(c.telegramChats) == 0 {
		return fmt.Errorf("no email -> chat id mapping: set TELEGRAM_CHAT_MAP or mount one at %s (TELEGRAM_CHAT_MAP_FILE)",
			c.telegramChatFile)
	}
	return nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// chatsFor returns the Telegram chat ids mapped to an address, normalising the
// same way parseChatMap normalised the keys. Multica stores addresses as typed,
// so the lookup cannot assume they arrive in the case used in the map.
func (c *config) chatsFor(email string) []string {
	return c.telegramChats[normalizeEmail(email)]
}

// testTarget picks the recipient for -send-test. An explicit address always
// wins; otherwise the map must have exactly one entry, so a multi-person setup
// cannot accidentally page someone who did not ask for it.
func (c *config) testTarget(email string) (string, []string, error) {
	if email = normalizeEmail(email); email != "" {
		chats := c.chatsFor(email)
		if len(chats) == 0 {
			return "", nil, fmt.Errorf("-send-test-email %q has no entry in TELEGRAM_CHAT_MAP", email)
		}
		return email, chats, nil
	}

	emails := sortedEmails(c.telegramChats)
	if len(emails) != 1 {
		return "", nil, fmt.Errorf("TELEGRAM_CHAT_MAP has %d addresses; set -send-test-email to one of: %s",
			len(emails), strings.Join(emails, ", "))
	}
	return emails[0], c.telegramChats[emails[0]], nil
}

// chatIDs decodes either one chat id or a list of them, so the common case
// stays terse while a shared address can fan out:
//
//	{"me@example.com": "123456789", "team@example.com": ["111", "222"]}
//
// A bare JSON number is accepted too because Telegram chat ids are numeric and
// quoting them is easy to forget.
type chatIDs []string

func (ids *chatIDs) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("empty chat id")
	}

	switch trimmed[0] {
	case '"':
		var one string
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*ids = chatIDs{one}
	case '[':
		var many []string
		if err := json.Unmarshal(trimmed, &many); err != nil {
			return err
		}
		*ids = many
	default:
		// json.Number keeps the digits verbatim instead of round-tripping an
		// int64 through float64.
		var n json.Number
		if err := json.Unmarshal(trimmed, &n); err != nil {
			return fmt.Errorf("chat id must be a string, number or array: %w", err)
		}
		*ids = chatIDs{n.String()}
	}
	return nil
}

// parseChatMap decodes TELEGRAM_CHAT_MAP into a lookup table. Keys are
// normalised to lower case (quoted numbers inside a JSON array are still
// strings, so chat ids stay strings as well).
func parseChatMap(raw string) (map[string][]string, error) {
	var decoded map[string]chatIDs
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("must be a JSON object of email -> chat id(s): %w", err)
	}
	if len(decoded) == 0 {
		return nil, errors.New("must contain at least one email -> chat id mapping")
	}

	out := make(map[string][]string, len(decoded))
	for rawEmail, rawIDs := range decoded {
		email := normalizeEmail(rawEmail)
		if email == "" {
			return nil, errors.New("contains an empty email key")
		}
		if _, duplicate := out[email]; duplicate {
			return nil, fmt.Errorf("email %q appears more than once (email keys are case-insensitive)", rawEmail)
		}

		ids := make([]string, 0, len(rawIDs))
		seen := make(map[string]struct{}, len(rawIDs))
		for _, rawID := range rawIDs {
			id := strings.TrimSpace(rawID)
			if id == "" {
				return nil, fmt.Errorf("email %q has an empty chat id", email)
			}
			if strings.ContainsAny(id, " \t\r\n") {
				return nil, fmt.Errorf("email %q has an invalid chat id %q", email, id)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("email %q has no chat ids", email)
		}
		out[email] = ids
	}
	return out, nil
}

func sortedEmails(chats map[string][]string) []string {
	emails := make([]string, 0, len(chats))
	for email := range chats {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return emails
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// maskToken shows just enough of a bot token to confirm which bot is wired up
// without putting a usable credential in a log line.
func maskToken(t string) string {
	if len(t) <= 12 {
		return "***"
	}
	return t[:10] + "..." + t[len(t)-4:]
}

var errShutdown = errors.New("context cancelled")
