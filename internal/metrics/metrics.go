// Package metrics exposes calendar-bridge's operational counters over HTTP in
// the Prometheus text exposition format, plus liveness and readiness probes.
//
// The format is written by hand rather than pulled in from a client library.
// calendar-bridge is a single self-hosted binary with four direct
// dependencies, and the handful of counters here do not justify adding
// prometheus/client_golang and its transitive tree to that. The output is
// plain text-format 0.0.4, which every Prometheus-compatible scraper reads.
//
// Nothing here records calendar contents. The exported series carry counts,
// timestamps, and account NAMES — the same short labels that already appear in
// the config and the logs. No event titles, no attendees, no calendar IDs, no
// credentials.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// namespace prefixes every series.
const namespace = "calendar_bridge"

// durationBuckets are the cumulative histogram bounds, in seconds, for a sync
// pass. A pass is normally well under a second and pathologically slow above a
// minute, so the buckets are dense at the bottom and sparse at the top.
var durationBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Pass is one sync pass's outcome, as handed to Observe. It is deliberately
// independent of internal/sync's Result so this package stays a leaf.
type Pass struct {
	Started  time.Time
	Duration time.Duration
	Created  int
	Updated  int
	Deleted  int
	Skipped  int
	// Healthy and Failed name the accounts whose fetch succeeded and failed.
	Healthy []string
	Failed  []string
	// Err is the pass's error, if any. A pass can be partially successful and
	// still carry an error.
	Err error
}

// BuildInfo identifies the running binary, exported as a build_info gauge so a
// dashboard can tell which version produced a given series.
type BuildInfo struct {
	Version   string
	Commit    string
	GoVersion string
}

// Registry accumulates the counters and serves them. The zero value is not
// usable; construct one with New.
type Registry struct {
	build BuildInfo
	now   func() time.Time

	mu sync.Mutex

	passesSucceeded uint64
	passesFailed    uint64
	created         uint64
	updated         uint64
	deleted         uint64
	skipped         uint64

	durationCounts []uint64 // cumulative-by-bucket counts, parallel to durationBuckets
	durationSum    float64
	durationCount  uint64

	accountErrors  map[string]uint64
	accountHealthy map[string]bool

	lastPass    time.Time
	lastSuccess time.Time
	started     time.Time
}

// New returns an empty Registry. now may be nil, in which case time.Now is
// used; injecting it keeps the readiness threshold testable.
func New(build BuildInfo, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	r := &Registry{
		build:          build,
		now:            now,
		durationCounts: make([]uint64, len(durationBuckets)),
		accountErrors:  make(map[string]uint64),
		accountHealthy: make(map[string]bool),
	}
	r.started = now()
	return r
}

// Observe records one completed sync pass.
func (r *Registry) Observe(p Pass) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p.Err == nil {
		r.passesSucceeded++
		r.lastSuccess = p.Started
	} else {
		r.passesFailed++
	}
	r.lastPass = p.Started

	r.created += uint64(max(p.Created, 0))
	r.updated += uint64(max(p.Updated, 0))
	r.deleted += uint64(max(p.Deleted, 0))
	r.skipped += uint64(max(p.Skipped, 0))

	secs := p.Duration.Seconds()
	if secs < 0 {
		secs = 0
	}
	r.durationSum += secs
	r.durationCount++
	for i, b := range durationBuckets {
		if secs <= b {
			r.durationCounts[i]++
		}
	}

	for _, a := range p.Healthy {
		r.accountHealthy[a] = true
		if _, ok := r.accountErrors[a]; !ok {
			r.accountErrors[a] = 0 // export a zero series so the account is visible
		}
	}
	for _, a := range p.Failed {
		r.accountHealthy[a] = false
		r.accountErrors[a]++
	}
}

// LastSuccess returns when the last fully-successful pass started, or the zero
// time if there has not been one.
func (r *Registry) LastSuccess() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSuccess
}

// ---- exposition ----

func (r *Registry) writeTo(sb *strings.Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()

	metric(sb, "build_info", "gauge",
		"Build identification for the running binary; always 1.",
		sample{labels: [][2]string{
			{"version", r.build.Version},
			{"commit", r.build.Commit},
			{"go_version", r.build.GoVersion},
		}, value: 1})

	metric(sb, "sync_passes_total", "counter",
		"Sync passes completed, by outcome.",
		sample{labels: [][2]string{{"outcome", "success"}}, value: float64(r.passesSucceeded)},
		sample{labels: [][2]string{{"outcome", "failure"}}, value: float64(r.passesFailed)})

	metric(sb, "blocks_total", "counter",
		"Busy blocks written, by action.",
		sample{labels: [][2]string{{"action", "created"}}, value: float64(r.created)},
		sample{labels: [][2]string{{"action", "updated"}}, value: float64(r.updated)},
		sample{labels: [][2]string{{"action", "deleted"}}, value: float64(r.deleted)})

	metric(sb, "events_skipped_total", "counter",
		"Source events deliberately not propagated because they were marked Free or declined.",
		sample{value: float64(r.skipped)})

	// Histogram: cumulative buckets, then +Inf, _sum and _count.
	header(sb, "sync_duration_seconds", "histogram", "Duration of a sync pass, in seconds.")
	for i, b := range durationBuckets {
		writeSample(sb, "sync_duration_seconds_bucket",
			[][2]string{{"le", strconv.FormatFloat(b, 'g', -1, 64)}}, float64(r.durationCounts[i]))
	}
	writeSample(sb, "sync_duration_seconds_bucket", [][2]string{{"le", "+Inf"}}, float64(r.durationCount))
	writeSample(sb, "sync_duration_seconds_sum", nil, r.durationSum)
	writeSample(sb, "sync_duration_seconds_count", nil, float64(r.durationCount))

	accounts := make([]string, 0, len(r.accountHealthy))
	for a := range r.accountHealthy {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts) // stable output; scrapers and diffs both prefer it

	if len(accounts) > 0 {
		header(sb, "account_healthy", "gauge",
			"1 if the account's events were fetched successfully on the last pass, 0 otherwise.")
		for _, a := range accounts {
			v := 0.0
			if r.accountHealthy[a] {
				v = 1
			}
			writeSample(sb, "account_healthy", [][2]string{{"account", a}}, v)
		}

		header(sb, "account_fetch_errors_total", "counter",
			"Passes in which this account's events could not be fetched.")
		for _, a := range accounts {
			writeSample(sb, "account_fetch_errors_total", [][2]string{{"account", a}}, float64(r.accountErrors[a]))
		}
	}

	metric(sb, "last_success_timestamp_seconds", "gauge",
		"Unix time of the last fully-successful sync pass; 0 if there has never been one. "+
			"Alert when time() minus this exceeds a few poll intervals.",
		sample{value: unix(r.lastSuccess)})

	metric(sb, "last_pass_timestamp_seconds", "gauge",
		"Unix time of the last sync pass, successful or not; 0 if none has run.",
		sample{value: unix(r.lastPass)})

	metric(sb, "start_time_seconds", "gauge",
		"Unix time at which this process started.",
		sample{value: unix(r.started)})
}

func unix(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}

type sample struct {
	labels [][2]string
	value  float64
}

func header(sb *strings.Builder, name, typ, help string) {
	fmt.Fprintf(sb, "# HELP %s_%s %s\n", namespace, name, escapeHelp(help))
	fmt.Fprintf(sb, "# TYPE %s_%s %s\n", namespace, name, typ)
}

func metric(sb *strings.Builder, name, typ, help string, samples ...sample) {
	header(sb, name, typ, help)
	for _, s := range samples {
		writeSample(sb, name, s.labels, s.value)
	}
}

func writeSample(sb *strings.Builder, name string, labels [][2]string, v float64) {
	sb.WriteString(namespace)
	sb.WriteByte('_')
	sb.WriteString(name)
	if len(labels) > 0 {
		sb.WriteByte('{')
		for i, kv := range labels {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(kv[0])
			sb.WriteString(`="`)
			sb.WriteString(escapeLabel(kv[1]))
			sb.WriteString(`"`)
		}
		sb.WriteByte('}')
	}
	sb.WriteByte(' ')
	sb.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	sb.WriteByte('\n')
}

// escapeLabel escapes a label value per the text exposition format: backslash,
// double quote and newline. Account names come from the operator's own config,
// but a name containing a quote would otherwise produce output no scraper can
// parse.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// escapeHelp escapes a HELP string: backslash and newline only.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// ---- handlers ----

// Options configures the metrics/health HTTP surface.
type Options struct {
	// ReadyMaxAge is how stale the last successful sync may be before /readyz
	// reports not-ready. Zero disables the staleness check, so readiness then
	// only reflects whether the process is up.
	ReadyMaxAge time.Duration
}

// Handler returns the HTTP handler serving /metrics, /healthz and /readyz.
//
// The surface is read-only and carries no authentication: bind it somewhere
// only your monitoring can reach (loopback, a private interface, or a
// container network), exactly as you would any other exporter.
func (r *Registry) Handler(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", r.handleMetrics)

	// Liveness: the process is running and its HTTP server is answering. It
	// deliberately does NOT consider sync health — a calendar-bridge that
	// cannot reach Google should be left alone to keep retrying, not killed
	// and restarted by an orchestrator.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlain(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		last := r.LastSuccess()
		if opts.ReadyMaxAge <= 0 {
			writePlain(w, http.StatusOK, "ok")
			return
		}
		if last.IsZero() {
			writePlain(w, http.StatusServiceUnavailable, "no successful sync pass yet")
			return
		}
		if age := r.now().Sub(last); age > opts.ReadyMaxAge {
			writePlain(w, http.StatusServiceUnavailable,
				fmt.Sprintf("last successful sync was %s ago, over the %s threshold",
					age.Truncate(time.Second), opts.ReadyMaxAge))
			return
		}
		writePlain(w, http.StatusOK, "ok")
	})

	return mux
}

func (r *Registry) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var sb strings.Builder
	r.writeTo(&sb)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
