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
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// defaultChannel is the Postgres notification channel the trigger publishes
// on. It is namespaced to this program so it cannot collide with anything else
// listening in the multica database.
const defaultChannel = "multica_verification_code"

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
	telegramChatID string
	telegramAPIURL string

	// Optional comma-separated allowlist. Empty means "notify for every
	// address", which is the right default only while this is the sole user of
	// the multica instance.
	emailAllowlist []string

	// Tuning.
	maxBackoff  time.Duration
	sendTimeout time.Duration
}

func main() {
	var (
		showConfig  = flag.Bool("show-config", false, "print the resolved configuration (secrets redacted) and exit")
		setupOnly   = flag.Bool("setup-only", false, "install the NOTIFY trigger, then exit without listening")
		sendTest    = flag.String("send-test", "", "send this text to the configured chat and exit (no database involvement)")
		listenTable = flag.String("table", "verification_code", "table to watch (without schema)")
		channelFlag = flag.String("channel", defaultChannel, "Postgres NOTIFY channel to listen on")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log, *showConfig, *setupOnly, *sendTest, *listenTable, *channelFlag); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, showConfig, setupOnly bool, sendTest, listenTable, channel string) error {
	cfg, err := loadConfig(listenTable, channel)
	if err != nil {
		return err
	}

	if showConfig {
		fmt.Printf("postgres      %s@%s:%s/%s (sslmode=%s)\n",
			cfg.pgUser, cfg.pgHost, cfg.pgPort, cfg.pgDatabase, cfg.pgSSLMode)
		fmt.Printf("watching      %s.%s\n", cfg.tableSchema, cfg.table)
		fmt.Printf("channel       %s\n", cfg.channel)
		fmt.Printf("telegram      chat %s via %s (token %s)\n",
			cfg.telegramChatID, cfg.telegramAPIURL, maskToken(cfg.telegramToken))
		fmt.Printf("allowlist     %s\n", orAny(cfg.emailAllowlist))
		return nil
	}

	// SIGINT/SIGTERM cancels ctx, which unwinds the WaitForNotification loop
	// and the reconnect backoff. Needed so a rollout does not wait out the
	// termination grace period.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tg := &telegramClient{
		token:   cfg.telegramToken,
		chatID:  cfg.telegramChatID,
		baseURL: strings.TrimRight(cfg.telegramAPIURL, "/"),
		client:  &http.Client{Timeout: cfg.sendTimeout},
	}

	// Smoke-test the Telegram credentials before touching the database, so a
	// bad token fails loudly at startup instead of silently on the first code.
	if err := tg.sendMessage(ctx, "multica-code-notifier connected. Waiting for login codes."); err != nil {
		return fmt.Errorf("telegram startup check failed: %w", err)
	}
	log.Info("telegram reachable", "chat_id", cfg.telegramChatID)

	if sendTest != "" {
		if err := tg.sendMessage(ctx, sendTest); err != nil {
			return fmt.Errorf("send test message: %w", err)
		}
		log.Info("test message sent", "chat_id", cfg.telegramChatID)
		return nil
	}

	notifier := &notifier{cfg: cfg, log: log, tg: tg}

	if setupOnly {
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
		telegramChatID: os.Getenv("TELEGRAM_CHAT_ID"),
		telegramAPIURL: env("TELEGRAM_API_URL", "https://api.telegram.org"),

		emailAllowlist: splitCSV(os.Getenv("NOTIFY_EMAIL_ALLOWLIST")),
		maxBackoff:     30 * time.Second,
		sendTimeout:    15 * time.Second,
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
	if c.telegramChatID == "" {
		missing = append(missing, "TELEGRAM_CHAT_ID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

// allowed reports whether a login code for this address should be relayed.
func (c *config) allowed(email string) bool {
	if len(c.emailAllowlist) == 0 {
		return true
	}
	for _, want := range c.emailAllowlist {
		if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(email)) {
			return true
		}
	}
	return false
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orAny(list []string) string {
	if len(list) == 0 {
		return "(any)"
	}
	return strings.Join(list, ", ")
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
