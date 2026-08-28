// Command calendar-bridge propagates busy time across multiple Google
// Calendar accounts without a third-party service seeing your calendar
// data.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/dcotelo/calendar-bridge/internal/config"
	"github.com/dcotelo/calendar-bridge/internal/googleauth"
	"github.com/dcotelo/calendar-bridge/internal/sync"
	"github.com/dcotelo/calendar-bridge/internal/webhook"
)

// syncCycleTimeout bounds a single SyncOnce pass so a hung Google API call
// can never wedge the process indefinitely. Generous relative to expected
// sync duration (seconds, even for many accounts/events) but still finite.
const syncCycleTimeout = 5 * time.Minute

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "auth":
		runAuth(os.Args[2:])
	case "run":
		runSync(os.Args[2:])
	case "sync-once":
		runSyncOnce(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `calendar-bridge - self-hosted busy-time sync across Google Calendar accounts

Usage:
  calendar-bridge auth -config config.yaml -account <name>
      Run the interactive OAuth2 flow for one account and cache its token.

  calendar-bridge sync-once -config config.yaml
      Run a single sync pass and exit. Useful for cron/testing.

  calendar-bridge run -config config.yaml
      Run the sync loop continuously, polling at the configured interval.
`)
}

func loadConfig(fs *flag.FlagSet, args []string) *config.Config {
	configPath := fs.String("config", "config.yaml", "path to config file")
	// fs was constructed with flag.ExitOnError, so Parse already exits the
	// process on a parse error; the returned error is always nil here.
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

func runAuth(args []string) {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	accountName := fs.String("account", "", "account name from config to authorize")
	_ = fs.Parse(args) // ExitOnError FlagSet: Parse exits on error, never returns one here

	if *accountName == "" {
		fmt.Fprintln(os.Stderr, "auth: -account is required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		os.Exit(1)
	}

	var target *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == *accountName {
			target = &cfg.Accounts[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "auth: no account named %q in config\n", *accountName)
		os.Exit(1)
	}

	ctx := context.Background()
	if err := googleauth.Authorize(ctx, target.CredentialsFile, target.TokenFile); err != nil {
		fmt.Fprintf(os.Stderr, "authorizing %s: %v\n", *accountName, err)
		os.Exit(1)
	}
}

func buildEngine(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*sync.Engine, map[string]*calendar.Service, error) {
	accounts := make([]sync.Account, 0, len(cfg.Accounts))
	services := make(map[string]*calendar.Service, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		svc, err := googleauth.Client(ctx, a.CredentialsFile, a.TokenFile, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("account %s: %w", a.Name, err)
		}
		services[a.Name] = svc
		accounts = append(accounts, sync.Account{
			Name:       a.Name,
			CalendarID: a.CalendarID,
			Client: sync.NewRetryingClient(
				sync.NewGoogleCalendarClient(svc),
				sync.DefaultRetryPolicy(),
				logger,
				a.Name,
			),
		})
	}

	return &sync.Engine{
		Accounts:      accounts,
		BlockTitle:    cfg.BlockTitle,
		LookaheadDays: cfg.LookaheadDays,
		Logger:        logger,
	}, services, nil
}

func runSyncOnce(args []string) {
	fs := flag.NewFlagSet("sync-once", flag.ExitOnError)
	cfg := loadConfig(fs, args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine, _, err := buildEngine(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setting up: %v\n", err)
		os.Exit(1)
	}

	cycleCtx, cancel := context.WithTimeout(ctx, syncCycleTimeout)
	defer cancel()
	if err := engine.SyncOnce(cycleCtx); err != nil {
		// A SIGINT/SIGTERM during this pass cancels ctx (and therefore
		// cycleCtx), which SyncOnce surfaces as an error. That's an
		// intentional, expected shutdown, not a failure — treat it the
		// same way the run loop does and exit 0. Genuine timeouts and API
		// errors still exit non-zero.
		if ctx.Err() != nil {
			logger.Info("received shutdown signal during sync, exiting")
			return
		}
		fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
		os.Exit(1)
	}
	logger.Info("sync pass complete")
}

func runSync(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg := loadConfig(fs, args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine, services, err := buildEngine(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setting up: %v\n", err)
		os.Exit(1)
	}

	interval, err := time.ParseDuration(cfg.PollInterval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid poll_interval %q: %v\n", cfg.PollInterval, err)
		os.Exit(1)
	}

	// Optional push path: when webhook is enabled, start the receiver and the
	// watch-channel manager. Push notifications trigger the same SyncOnce as
	// polling — polling stays on as a safety net so a missed/late notification
	// never means a permanently-missed change.
	var pushTrigger <-chan struct{}
	if cfg.Webhook.Enabled {
		trigger, err := startWebhook(ctx, cfg, services, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "starting webhook: %v\n", err)
			os.Exit(1)
		}
		pushTrigger = trigger
		logger.Info("push notifications enabled", "listen", cfg.Webhook.ListenAddr, "public_url", cfg.Webhook.PublicURL)
	}

	logger.Info("starting sync loop", "interval", interval, "accounts", len(cfg.Accounts), "push", cfg.Webhook.Enabled)
	for {
		select {
		case <-ctx.Done():
			logger.Info("received shutdown signal, exiting")
			return
		default:
		}

		cycleCtx, cancel := context.WithTimeout(ctx, syncCycleTimeout)
		err := engine.SyncOnce(cycleCtx)
		cancel()
		if err != nil {
			logger.Error("sync pass failed", "error", err)
		} else {
			logger.Info("sync pass complete")
		}

		// Wait for the next trigger: a poll tick, a debounced push
		// notification, or shutdown — whichever comes first.
		select {
		case <-ctx.Done():
			logger.Info("received shutdown signal, exiting")
			return
		case <-pushTrigger:
			logger.Info("sync triggered by push notification")
		case <-time.After(interval):
		}
	}
}

// startWebhook starts the push-notification receiver HTTP server and the
// watch-channel manager, returning a channel that fires (debounced) whenever a
// calendar change notification arrives. Both the server and manager stop when
// ctx is cancelled.
func startWebhook(ctx context.Context, cfg *config.Config, services map[string]*calendar.Service, logger *slog.Logger) (<-chan struct{}, error) {
	debounceInterval, err := time.ParseDuration(cfg.Webhook.DebounceInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook.debounce_interval %q: %w", cfg.Webhook.DebounceInterval, err)
	}
	ttl, err := time.ParseDuration(cfg.Webhook.ChannelTTL)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook.channel_ttl %q: %w", cfg.Webhook.ChannelTTL, err)
	}

	debouncer := webhook.NewDebouncer(debounceInterval)
	receiver := webhook.NewReceiver(cfg.Webhook.VerificationToken, debouncer, logger)

	mux := http.NewServeMux()
	mux.Handle("/webhook", receiver)
	srv := &http.Server{
		Addr:              cfg.Webhook.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("webhook server stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// Watch-channel manager: register/renew a channel per account calendar.
	watcher := webhook.NewGoogleWatcher(services)
	callbackURL := strings.TrimRight(cfg.Webhook.PublicURL, "/") + "/webhook"
	mgr := webhook.NewManager(watcher, callbackURL, cfg.Webhook.VerificationToken, ttl, logger)

	targets := make([]webhook.Target, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		targets = append(targets, webhook.Target{Account: a.Name, CalendarID: a.CalendarID})
	}
	go func() { _ = mgr.Run(ctx, targets) }()

	return debouncer.C, nil
}
