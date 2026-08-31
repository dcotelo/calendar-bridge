// Command calendar-bridge propagates busy time across multiple Google
// Calendar accounts without a third-party service seeing your calendar
// data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

// Exit codes. Documented so scripts and supervisors can branch on them.
const (
	exitOK          = 0
	exitUsage       = 2 // bad flags, unknown command, missing required argument
	exitConfig      = 3 // config file missing, unparseable, or invalid
	exitAuth        = 4 // an account is unauthorized, or its token is unreadable
	exitSyncFailure = 5 // the pass ran but at least one account or write failed
	exitRuntime     = 6 // could not start (port in use, listener refused, etc.)
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(exitUsage)
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
	case "version", "-version", "--version":
		printVersion(os.Stdout)
	case "-h", "--help", "help":
		// Asked-for help is output, not an error: stdout, exit 0, so
		// `calendar-bridge --help | less` works.
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(exitUsage)
	}
}

// parseFlags parses args, routing an explicit help request to STDOUT with exit
// 0 and any other parse error to STDERR with exit 2.
//
// flag.ExitOnError sends -h output to stderr and exits 0, which makes
// `calendar-bridge sync-once -h | less` show nothing. The obvious fix — scan
// args for -h before parsing — is wrong: it would accept `sync-once -bogus -h`
// and print help, when the flag package's left-to-right parse should fail on
// -bogus first. ContinueOnError keeps that ordering exactly and lets the
// destination be chosen per outcome.
func parseFlags(fs *flag.FlagSet, args []string) {
	// Suppress the package's own printing; the error paths below choose the
	// stream and the message.
	fs.SetOutput(io.Discard)

	err := fs.Parse(args)
	switch {
	case err == nil:
		// Parse stops at the first non-flag argument and leaves the rest in
		// Args(). No subcommand takes positional arguments — auth names its
		// account with -account — so anything left is a typo, and silently
		// ignoring it is dangerous rather than merely untidy: in
		// `sync-once typo -dry-run` the parse stops at typo, -dry-run is
		// never seen, and a real sync writes to live calendars when the
		// operator asked for a dry run.
		if rest := fs.Args(); len(rest) > 0 {
			fs.SetOutput(os.Stderr)
			_, _ = fmt.Fprintf(os.Stderr,
				"%s: unexpected argument %q\n\nUsage of %s:\n", fs.Name(), rest[0], fs.Name())
			fs.PrintDefaults()
			os.Exit(exitUsage)
		}
		return
	case errors.Is(err, flag.ErrHelp):
		// Asked-for help is output, not an error.
		fs.SetOutput(os.Stdout)
		_, _ = fmt.Fprintf(os.Stdout, "Usage of %s:\n", fs.Name())
		fs.PrintDefaults()
		os.Exit(exitOK)
	default:
		fs.SetOutput(os.Stderr)
		_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n\nUsage of %s:\n", fs.Name(), err, fs.Name())
		fs.PrintDefaults()
		os.Exit(exitUsage)
	}
}

// usage writes the help text. A failed write to stdout or stderr has no
// useful recovery, so the error is deliberately ignored.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `calendar-bridge - self-hosted busy-time sync across Google Calendar accounts

Usage:
  calendar-bridge auth -config config.yaml -account <name>
      Run the interactive OAuth2 flow for one account and cache its token.
      Do this once per account, on a machine with a browser, before `+"`run`"+`.

  calendar-bridge sync-once [-config config.yaml] [-dry-run] [-json]
      Run a single sync pass and exit. Useful for cron, and for checking a
      config before leaving it running.

  calendar-bridge run [-config config.yaml] [-dry-run]
      Run the sync loop continuously, polling at the configured interval.
      Exits cleanly on SIGINT/SIGTERM.

  calendar-bridge ui [-config config.yaml]
      Serve the local configuration web UI. Loopback-only: a non-loopback
      listen_addr is refused. Requires web_ui.enabled: true in the config.

  calendar-bridge version
      Print version, commit, build date, Go version and platform.

Flags:
  -config <path>   Path to the config file (default "config.yaml").
  -dry-run         Report the blocks that would be created, moved and removed
                   without writing anything. Reads still hit the Calendar API,
                   so working credentials are still required.
  -json            Emit the pass result as a single JSON object on stdout
                   (sync-once only). Logs continue to go to stderr.

Exit codes:
  0  success
  2  usage error (bad flags, unknown command, missing argument)
  3  configuration error
  4  an account needs authorization, or its token file is unreadable
  5  the sync pass ran but reported failures
  6  could not start (address in use, listener refused)

Docs: https://github.com/dcotelo/calendar-bridge
`)
}

// parseAndLoad parses args and loads the config, returning the error rather
// than exiting. Callers that need to emit something before dying (sync-once
// with -json) use this; the rest use loadConfig.
func parseAndLoad(fs *flag.FlagSet, args []string) (*config.Config, error) {
	configPath := fs.String("config", "config.yaml", "path to config file")
	parseFlags(fs, args)
	return config.Load(*configPath)
}

func loadConfig(fs *flag.FlagSet, args []string) *config.Config {
	cfg, err := parseAndLoad(fs, args)
	if err != nil {
		// The error is deliberately dropped rather than printed. This is a
		// daemon path — under systemd stderr is the journal, under Docker it
		// is `docker logs` — so the on-disk layout stays out of it, and the
		// operator passed the path on the command line anyway.
		//
		// config.Load is independently path-free (it strips the *fs.PathError
		// cause), so printing err would be safe; the message below is kept
		// stable on purpose instead. See reportConfigError.
		reportConfigError()
		os.Exit(exitConfig)
	}
	return cfg
}

// reportConfigError prints a stable, path-free message for an unloadable
// config. The distinction between "missing" and "invalid" is deliberately not
// drawn here: both are fixed by looking at the file the operator named.
func reportConfigError() {
	fmt.Fprintln(os.Stderr, "loading config: could not read or parse the config file "+
		"(check the -config path and its contents)")
}

func runAuth(args []string) {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	accountName := fs.String("account", "", "account name from config to authorize")
	parseFlags(fs, args)

	if *accountName == "" {
		fmt.Fprintln(os.Stderr, "auth: -account is required\n\nExample:\n  calendar-bridge auth -config config.yaml -account personal")
		os.Exit(exitUsage)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		reportConfigError()
		os.Exit(exitConfig)
	}

	var target *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == *accountName {
			target = &cfg.Accounts[i]
			break
		}
	}
	if target == nil {
		names := make([]string, 0, len(cfg.Accounts))
		for _, a := range cfg.Accounts {
			names = append(names, a.Name)
		}
		fmt.Fprintf(os.Stderr, "auth: no account named %q in config; configured accounts are: %s\n",
			*accountName, strings.Join(names, ", "))
		os.Exit(exitUsage)
	}

	ctx := context.Background()
	if err := googleauth.Authorize(ctx, target.CredentialsFile, target.TokenFile); err != nil {
		// Named by ACCOUNT, which is what the operator acts on. googleauth
		// keeps DIRECTORIES out of its errors; a bare base name may remain and
		// is deliberately kept, because it identifies which of several token
		// files is at fault without disclosing where they live.
		fmt.Fprintf(os.Stderr, "authorizing account %s failed: %v\n", *accountName, err)
		os.Exit(exitAuth)
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
		// Client stack, outermost first:
		//
		//   providerClient   neutral Provider -> CalendarClient bridge
		//   googleProvider   ownership enforcement: refuses to insert or update
		//                    an untagged block, and re-reads + re-verifies the
		//                    target before every delete
		//   retryingClient   429/5xx/network-timeout backoff, and insert
		//                    reconciliation after an ambiguous failure
		//   googleCalendar   the real Calendar API
		//
		// Retry sits BELOW the provider so each individual Google call the
		// provider makes (the pre-delete read as well as the delete) gets its
		// own backoff, rather than a retry replaying a whole check-and-write.
		retrying := sync.NewRetryingClient(
			sync.NewGoogleCalendarClient(svc),
			sync.DefaultRetryPolicy(),
			logger,
			a.Name,
		)
		accounts = append(accounts, sync.Account{
			Name:       a.Name,
			CalendarID: a.CalendarID,
			Client:     sync.NewProviderClient(sync.NewGoogleProvider(retrying)),
		})
	}

	return &sync.Engine{
		Accounts:      accounts,
		BlockTitle:    cfg.BlockTitle,
		LookaheadDays: cfg.LookaheadDays,
		Logger:        logger,
	}, services, nil
}

// reportSetupError prints a setup failure with an actionable next step for the
// two causes an operator can actually fix themselves, rather than only the
// wrapped error text.
func reportSetupError(err error) int {
	fmt.Fprintf(os.Stderr, "setting up: %v\n", err)
	switch {
	case errors.Is(err, googleauth.ErrNeedsAuth):
		fmt.Fprintln(os.Stderr, "\nRun the authorization flow for that account:\n  calendar-bridge auth -config <config.yaml> -account <name>")
		return exitAuth
	case errors.Is(err, googleauth.ErrTokenInaccessible):
		fmt.Fprintln(os.Stderr, "\nThe token file exists but could not be opened — check its ownership and\n"+
			"permissions, and those of the directory holding it. It must be readable AND\n"+
			"writable by the user running calendar-bridge (refreshed tokens are written back).")
		return exitAuth
	case errors.Is(err, googleauth.ErrTokenUnreadable):
		fmt.Fprintln(os.Stderr, "\nThe token file is present but corrupt (an interrupted write, or hand-edited).\n"+
			"Delete it and re-run the authorization flow:\n  calendar-bridge auth -config <config.yaml> -account <name>")
		return exitAuth
	}
	return exitRuntime
}

// passReport is the -json shape of a single sync pass. Counts, timings and
// account names only — no event data.
type passReport struct {
	Version string `json:"version"`
	DryRun  bool   `json:"dry_run"`
	OK      bool   `json:"ok"`
	// Interrupted reports a pass cut short by SIGINT/SIGTERM. That exits 0,
	// because it is an intentional shutdown rather than a failure — so without
	// this a consumer could not tell it apart from a genuine sync error, which
	// exits 5. When true, OK is true and Error is empty.
	Interrupted bool     `json:"interrupted,omitempty"`
	Error       string   `json:"error,omitempty"`
	StartedAt   string   `json:"started_at"`
	DurationMS  int64    `json:"duration_ms"`
	Created     int      `json:"created"`
	Updated     int      `json:"updated"`
	Deleted     int      `json:"deleted"`
	Skipped     int      `json:"skipped"`
	Healthy     []string `json:"healthy_accounts"`
	Failed      []string `json:"failed_accounts,omitempty"`
}

// emitFailureReport writes the -json object for a failure that happened before
// a sync pass could run, so stdout still carries exactly one decodable object.
//
// The counts are zero and the duration is zero because no pass took place —
// that is the honest report, and it is distinguishable from a pass that ran
// and changed nothing by OK being false with a non-empty error.
//
// err is safe to include: config.Load and the googleauth setup errors are
// path-free by construction, which their own tests assert.
func emitFailureReport(dryRun bool, err error) {
	rep := passReport{
		Version:   versionString(),
		DryRun:    dryRun,
		OK:        false,
		Error:     err.Error(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(rep); encErr != nil {
		fmt.Fprintf(os.Stderr, "writing JSON report: %v\n", encErr)
	}
}

// wasInterrupted reports whether a failed pass was cut short by SIGINT/SIGTERM
// rather than by a genuine error. syncErr is what SyncOnce returned; ctxErr is
// the state of the *signal* context (not the per-cycle timeout context).
//
// Both conditions are load-bearing:
//
//   - ctxErr != nil alone would misclassify any failure that happened to land
//     just before a signal: a 401 returning microseconds before SIGINT would
//     exit 0 and look like a clean shutdown, silently hiding a broken account.
//   - errors.Is(syncErr, context.Canceled) alone would treat the per-cycle
//     timeout as a shutdown, because a timed-out cycle also cancels. The
//     signal context is the one that distinguishes "operator asked to stop"
//     from "this pass took too long", and a timeout must exit non-zero.
//
// A pass that genuinely failed and was then cancelled wraps both, and counts
// as interrupted: the operator asked it to stop, and the next run re-reports
// anything still wrong.
func wasInterrupted(syncErr, ctxErr error) bool {
	return syncErr != nil && ctxErr != nil && errors.Is(syncErr, context.Canceled)
}

func runSyncOnce(args []string) {
	fs := flag.NewFlagSet("sync-once", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing to any calendar")
	asJSON := fs.Bool("json", false, "emit the pass result as JSON on stdout")
	cfg, cfgErr := parseAndLoad(fs, args)

	// With -json, stdout carries exactly one JSON object; logs go to stderr so
	// the output stays machine-readable.
	logDest := io.Writer(os.Stdout)
	if *asJSON {
		logDest = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logDest, nil))

	// A -json consumer gets a decodable object on EVERY exit, including the
	// two that happen before a pass can start. Emitting nothing on the most
	// common failures — an unreadable config, an account that needs
	// authorization — forces the consumer to parse stderr or infer from the
	// exit code alone.
	if cfgErr != nil {
		reportConfigError()
		if *asJSON {
			emitFailureReport(*dryRun, cfgErr)
		}
		os.Exit(exitConfig)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine, _, err := buildEngine(ctx, cfg, logger)
	if err != nil {
		// reportSetupError writes the operator-facing guidance to stderr and
		// classifies the exit code; the JSON object is the machine-facing
		// half of the same failure.
		code := reportSetupError(err)
		if *asJSON {
			emitFailureReport(*dryRun, err)
		}
		os.Exit(code)
	}
	engine.DryRun = *dryRun

	cycleCtx, cancel := context.WithTimeout(ctx, syncCycleTimeout)
	defer cancel()
	res, syncErr := engine.SyncOnce(cycleCtx)

	interrupted := wasInterrupted(syncErr, ctx.Err())

	if *asJSON {
		rep := passReport{
			Version:     versionString(),
			DryRun:      *dryRun,
			OK:          syncErr == nil || interrupted,
			Interrupted: interrupted,
			StartedAt:   res.Started.UTC().Format(time.RFC3339),
			DurationMS:  res.Duration().Milliseconds(),
			Created:     res.Created, Updated: res.Updated,
			Deleted: res.Deleted, Skipped: res.Skipped,
			Healthy: res.HealthyAccounts, Failed: res.FailedAccounts,
		}
		if syncErr != nil && !interrupted {
			rep.Error = syncErr.Error()
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(rep); encErr != nil {
			fmt.Fprintf(os.Stderr, "writing JSON report: %v\n", encErr)
			os.Exit(exitRuntime)
		}
	}

	switch {
	case interrupted:
		logger.Info("received shutdown signal during sync, exiting")
	case syncErr != nil:
		if !*asJSON {
			fmt.Fprintf(os.Stderr, "sync failed: %v\n", syncErr)
		}
		os.Exit(exitSyncFailure)
	case !*asJSON:
		logger.Info("sync pass complete", "dry_run", *dryRun,
			"created", res.Created, "updated", res.Updated, "deleted", res.Deleted,
			"skipped", res.Skipped, "duration", res.Duration())
	}
}

func runSync(args []string) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing to any calendar")
	cfg := loadConfig(fs, args)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	state := newSyncState(cfg.Webhook.Enabled)

	engine, services, err := buildEngine(ctx, cfg, logger)
	if err != nil {
		os.Exit(reportSetupError(err))
	}
	engine.DryRun = *dryRun
	if *dryRun {
		logger.Warn("dry-run mode: no calendar will be written to")
	}

	interval, err := time.ParseDuration(cfg.PollInterval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid poll_interval %q: %v\n", cfg.PollInterval, err)
		os.Exit(exitConfig)
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
			os.Exit(exitRuntime)
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

	state.markRunning()
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
		res, err := engine.SyncOnce(cycleCtx)
		cancel()
		state.record(res, err)
		if err != nil {
			logger.Error("sync pass failed", "error", err)
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
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	parseFlags(fs, args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		// The message is deliberately stable and path-free: stderr is a
		// shared log (the journal under systemd, `docker logs` under Docker)
		// and the operator already knows the path they passed via -config.
		// config.Load is itself path-free, so this is belt-and-braces.
		fmt.Fprintln(os.Stderr, "ui: failed to load config (check -config path and file contents)")
		os.Exit(exitConfig)
	}
	if !cfg.WebUI.Enabled {
		fmt.Fprintln(os.Stderr, "ui: web_ui.enabled is false in config; set it to true to run the UI")
		os.Exit(exitConfig)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// `ui` serves the config surface only; it runs no background loop, so
	// Running stays false and the page says so rather than implying a daemon
	// is polling behind it.
	state := newSyncState(cfg.Webhook.Enabled)

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
			state.record(sync.Result{}, err)
			return fmt.Errorf("setting up sync: %w", err)
		}
		res, err := engine.SyncOnce(syncCtx)
		state.record(res, err)
		return err
	}

	statusFn := func() webui.Status {
		current, err := config.Load(*configPath)
		if err != nil {
			// Keep the path out of both the shared log and the reported
			// status — this runs on every page load, sync and reload, and
			// the status is rendered in a browser. config.Load is itself
			// path-free, so this is belt-and-braces.
			logger.Warn("webui: status could not load config")
			return webui.Status{LastError: "config load failed (check -config path and file contents)"}
		}
		return state.status(len(current.Accounts))
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
		os.Exit(exitConfig)
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
		os.Exit(exitRuntime)
	}
}
