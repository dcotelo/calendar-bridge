// Command calendar-bridge propagates busy time across multiple Google
// Calendar accounts without a third-party service seeing your calendar
// data.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	stdsync "sync"
	"syscall"
	"time"

	calendar "google.golang.org/api/calendar/v3"

	"github.com/dcotelo/calendar-bridge/internal/config"
	"github.com/dcotelo/calendar-bridge/internal/googleauth"
	"github.com/dcotelo/calendar-bridge/internal/sync"
	"github.com/dcotelo/calendar-bridge/internal/webhook"
	"github.com/dcotelo/calendar-bridge/internal/webui"
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
	case "ui":
		runUI(os.Args[2:])
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

  calendar-bridge ui -config config.yaml
      Serve the local configuration web UI (loopback-only by default; set
      web_ui.auth_token to expose it with authentication).
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
	waitWebhook := func() {}
	if cfg.Webhook.Enabled {
		trigger, wait, err := startWebhook(ctx, cfg, services, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "starting webhook: %v\n", err)
			os.Exit(1)
		}
		pushTrigger = trigger
		waitWebhook = wait
		// Log only the scheme://host origin, never the full URL: a public_url
		// may carry a non-root path we shouldn't echo into shared logs.
		origin := cfg.Webhook.PublicURL
		if u, err := url.Parse(cfg.Webhook.PublicURL); err == nil && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
		}
		logger.Info("push notifications enabled", "listen", cfg.Webhook.ListenAddr, "public_url_origin", origin)
	}

	logger.Info("starting sync loop", "interval", interval, "accounts", len(cfg.Accounts), "push", cfg.Webhook.Enabled)
	for {
		select {
		case <-ctx.Done():
			logger.Info("received shutdown signal, exiting")
			waitWebhook()
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
			waitWebhook()
			return
		case <-pushTrigger:
			logger.Info("sync triggered by push notification")
		case <-time.After(interval):
		}
	}
}

// startWebhook starts the push-notification receiver HTTP server and the
// watch-channel manager, returning a channel that fires (debounced) whenever a
// calendar change notification arrives, and a wait function that blocks until
// both the server and the manager have finished shutting down. Both stop when
// ctx is cancelled; the caller must call wait before the process exits, or
// Google may keep POSTing to a callback whose process has already gone away
// and watch channels may be left registered.
func startWebhook(ctx context.Context, cfg *config.Config, services map[string]*calendar.Service, logger *slog.Logger) (trigger <-chan struct{}, wait func(), err error) {
	debounceInterval, err := time.ParseDuration(cfg.Webhook.DebounceInterval)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid webhook.debounce_interval %q: %w", cfg.Webhook.DebounceInterval, err)
	}
	// Empty channel_ttl means "use the provider default" (ttl == 0); only parse
	// a non-empty value, since time.ParseDuration("") errors.
	var ttl time.Duration
	if cfg.Webhook.ChannelTTL != "" {
		ttl, err = time.ParseDuration(cfg.Webhook.ChannelTTL)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid webhook.channel_ttl %q: %w", cfg.Webhook.ChannelTTL, err)
		}
	}

	debouncer := webhook.NewDebouncer(debounceInterval)
	receiver := webhook.NewReceiver(cfg.Webhook.VerificationToken, debouncer, logger)

	mux := http.NewServeMux()
	mux.Handle("/webhook", receiver)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// The endpoint is publicly reachable; bound how long a stalled
		// client can hold a connection/goroutine open.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	// Bind synchronously so a failure (e.g. the port is already in use) is
	// reported to the caller instead of being swallowed in a goroutine — which
	// would otherwise leave the process polling and registering Google watch
	// channels whose callbacks can never be delivered.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.Webhook.ListenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("binding webhook listener on %s: %w", cfg.Webhook.ListenAddr, err)
	}

	var wg stdsync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("webhook server stopped", "error", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		// ctx is already cancelled (that's what unblocked us); derive a fresh
		// context that ignores that cancellation so Shutdown gets the full
		// grace period to drain instead of aborting immediately.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = mgr.Run(ctx, targets)
	}()

	return debouncer.C, wg.Wait, nil
}

func runUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		// config.Load embeds the path in its error; keep it out of stderr
		// (shared logs) and emit a stable message. The operator knows the path
		// they passed via -config.
		fmt.Fprintln(os.Stderr, "ui: failed to load config (check -config path and file contents)")
		os.Exit(1)
	}
	if !cfg.WebUI.Enabled {
		fmt.Fprintln(os.Stderr, "ui: web_ui.enabled is false in config; set it to true to run the UI")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// syncNow builds an engine on demand and runs a single pass. Building per
	// invocation keeps token/credential reads fresh (e.g. after re-auth) and
	// avoids holding calendar clients open while the UI idles.
	syncNow := func() error {
		syncCtx, cancel := context.WithTimeout(ctx, syncCycleTimeout)
		defer cancel()
		// Reload config so a just-saved change takes effect without a restart.
		current, err := config.Load(*configPath)
		if err != nil {
			return fmt.Errorf("reloading config: %w", err)
		}
		engine, _, err := buildEngine(syncCtx, current, logger)
		if err != nil {
			return fmt.Errorf("setting up sync: %w", err)
		}
		return engine.SyncOnce(syncCtx)
	}

	statusFn := func() webui.Status {
		current, err := config.Load(*configPath)
		if err != nil {
			// config.Load embeds the config path in its error text; keep that
			// out of both the shared log and the reported status (this runs
			// on every page load, sync, and reload) and log/report a stable,
			// path-free message instead.
			logger.Warn("webui: status could not load config")
			return webui.Status{LastError: "config load failed (check -config path and file contents)"}
		}
		return webui.Status{AccountsNum: len(current.Accounts)}
	}

	srv, err := webui.New(webui.Options{
		ConfigPath: *configPath,
		AuthToken:  cfg.WebUI.AuthToken,
		ListenAddr: cfg.WebUI.ListenAddr,
		Sync:       syncNow,
		Status:     statusFn,
		Logger:     logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ui: %v\n", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              cfg.WebUI.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Bound the full request read (headers + body) so a slow client can't
		// hold a connection open indefinitely trickling the body. The only
		// body is a small JSON config (capped at 1MiB in the handler), so 30s
		// is ample.
		ReadTimeout: 30 * time.Second,
		// Bound idle keep-alive connections. No short WriteTimeout on purpose:
		// POST /api/sync runs a full sync pass inline (up to syncCycleTimeout),
		// and a short write deadline would truncate a legitimate response.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	authNote := "no auth token set (loopback only)"
	if cfg.WebUI.AuthToken != "" {
		authNote = "auth token required"
	}
	logger.Info("serving configuration UI", "addr", cfg.WebUI.ListenAddr, "auth", authNote)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "ui: server error: %v\n", err)
		os.Exit(1)
	}
}
