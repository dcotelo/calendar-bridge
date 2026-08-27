// Command calendar-bridge propagates busy time across multiple Google
// Calendar accounts without a third-party service seeing your calendar
// data.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dcotelo/calendar-bridge/internal/config"
	"github.com/dcotelo/calendar-bridge/internal/googleauth"
	"github.com/dcotelo/calendar-bridge/internal/sync"
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
	fs.Parse(args)

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
	fs.Parse(args)

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

func buildEngine(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*sync.Engine, error) {
	accounts := make([]sync.Account, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		svc, err := googleauth.Client(ctx, a.CredentialsFile, a.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", a.Name, err)
		}
		accounts = append(accounts, sync.Account{
			Name:       a.Name,
			CalendarID: a.CalendarID,
			Client:     sync.NewGoogleCalendarClient(svc),
		})
	}

	return &sync.Engine{
		Accounts:      accounts,
		BlockTitle:    cfg.BlockTitle,
		LookaheadDays: cfg.LookaheadDays,
		Logger:        logger,
	}, nil
}

func runSyncOnce(args []string) {
	fs := flag.NewFlagSet("sync-once", flag.ExitOnError)
	cfg := loadConfig(fs, args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine, err := buildEngine(ctx, cfg, logger)
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

	engine, err := buildEngine(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setting up: %v\n", err)
		os.Exit(1)
	}

	interval, err := time.ParseDuration(cfg.PollInterval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid poll_interval %q: %v\n", cfg.PollInterval, err)
		os.Exit(1)
	}

	logger.Info("starting sync loop", "interval", interval, "accounts", len(cfg.Accounts))
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

		select {
		case <-ctx.Done():
			logger.Info("received shutdown signal, exiting")
			return
		case <-time.After(interval):
		}
	}
}
