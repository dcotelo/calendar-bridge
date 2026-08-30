package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testBuild() BuildInfo {
	return BuildInfo{Version: "v1.2.3", Commit: "abc1234", GoVersion: "go1.26.7"}
}

var base = time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.Handler(Options{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", w.Code)
	}
	return w.Body.String()
}

// sampleValue extracts the value of one exposed series.
func sampleValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, val, ok := strings.Cut(line, " ")
		if !ok || name != series {
			continue
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			t.Fatalf("series %s has unparseable value %q", series, val)
		}
		return f
	}
	t.Fatalf("series %q not found in:\n%s", series, body)
	return 0
}

func hasSeries(body, series string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "#") && strings.HasPrefix(line, series+" ") {
			return true
		}
	}
	return false
}

func TestRegistry_EmptyExpositionIsWellFormed(t *testing.T) {
	body := scrape(t, New(testBuild(), func() time.Time { return base }))
	assertValidExposition(t, body)

	// A never-synced instance must still export the timestamp series, at 0, so
	// an alert on staleness fires rather than going blind on a missing series.
	if got := sampleValue(t, body, "calendar_bridge_last_success_timestamp_seconds"); got != 0 {
		t.Errorf("last_success_timestamp_seconds = %v, want 0", got)
	}
	if !strings.Contains(body, `calendar_bridge_build_info{version="v1.2.3",commit="abc1234",go_version="go1.26.7"} 1`) {
		t.Errorf("build_info series is missing or malformed:\n%s", body)
	}
}

func TestRegistry_ObserveAccumulates(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })

	r.Observe(Pass{
		Started: base, Duration: 400 * time.Millisecond,
		Created: 3, Updated: 1, Deleted: 2, Skipped: 4,
		Healthy: []string{"personal", "work-acme"},
	})
	r.Observe(Pass{
		Started: base.Add(5 * time.Minute), Duration: 2 * time.Second,
		Created: 1,
		Healthy: []string{"personal"}, Failed: []string{"work-acme"},
		Err: errors.New("token expired"),
	})

	body := scrape(t, r)
	assertValidExposition(t, body)

	for _, tc := range []struct {
		series string
		want   float64
	}{
		{`calendar_bridge_sync_passes_total{outcome="success"}`, 1},
		{`calendar_bridge_sync_passes_total{outcome="failure"}`, 1},
		{`calendar_bridge_blocks_total{action="created"}`, 4},
		{`calendar_bridge_blocks_total{action="updated"}`, 1},
		{`calendar_bridge_blocks_total{action="deleted"}`, 2},
		{`calendar_bridge_events_skipped_total`, 4},
		{`calendar_bridge_account_healthy{account="personal"}`, 1},
		{`calendar_bridge_account_healthy{account="work-acme"}`, 0},
		{`calendar_bridge_account_fetch_errors_total{account="work-acme"}`, 1},
		{`calendar_bridge_account_fetch_errors_total{account="personal"}`, 0},
		{`calendar_bridge_sync_duration_seconds_count`, 2},
	} {
		if got := sampleValue(t, body, tc.series); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.series, got, tc.want)
		}
	}

	// The failed pass must not advance last_success.
	if got := sampleValue(t, body, "calendar_bridge_last_success_timestamp_seconds"); got != float64(base.Unix()) {
		t.Errorf("last_success = %v, want the first (successful) pass at %v", got, base.Unix())
	}
	if got := sampleValue(t, body, "calendar_bridge_last_pass_timestamp_seconds"); got != float64(base.Add(5*time.Minute).Unix()) {
		t.Errorf("last_pass = %v, want the second pass", got)
	}
}

// Cumulative buckets must be monotonically non-decreasing and end at _count,
// or Prometheus rejects the histogram.
func TestRegistry_HistogramIsCumulative(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })
	for _, d := range []time.Duration{
		50 * time.Millisecond, 300 * time.Millisecond, 900 * time.Millisecond,
		4 * time.Second, 45 * time.Second, 10 * time.Minute,
	} {
		r.Observe(Pass{Started: base, Duration: d, Healthy: []string{"a", "b"}})
	}

	body := scrape(t, r)
	assertValidExposition(t, body)

	re := regexp.MustCompile(`calendar_bridge_sync_duration_seconds_bucket\{le="([^"]+)"\} (\S+)`)
	matches := re.FindAllStringSubmatch(body, -1)
	if len(matches) != len(durationBuckets)+1 {
		t.Fatalf("got %d buckets, want %d (including +Inf)", len(matches), len(durationBuckets)+1)
	}

	var prev float64
	for i, m := range matches {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("bucket %s has unparseable count %q", m[1], m[2])
		}
		if v < prev {
			t.Errorf("bucket le=%s count %v is lower than the previous bucket's %v; buckets must be cumulative", m[1], v, prev)
		}
		prev = v
		if i == len(matches)-1 && m[1] != "+Inf" {
			t.Errorf("last bucket is le=%s, want +Inf", m[1])
		}
	}

	count := sampleValue(t, body, "calendar_bridge_sync_duration_seconds_count")
	if prev != count {
		t.Errorf("+Inf bucket = %v but _count = %v; they must agree", prev, count)
	}
	if got := sampleValue(t, body, "calendar_bridge_sync_duration_seconds_sum"); got <= 0 {
		t.Errorf("_sum = %v, want a positive total", got)
	}
}

// An account name containing a quote or backslash must not produce output a
// scraper cannot parse.
func TestRegistry_EscapesLabelValues(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })
	r.Observe(Pass{
		Started: base,
		Healthy: []string{`we"ird`, `back\slash`},
		Failed:  []string{"news\nline"},
	})

	body := scrape(t, r)
	assertValidExposition(t, body)

	for _, want := range []string{`account="we\"ird"`, `account="back\\slash"`, `account="news\nline"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected escaped label %s in:\n%s", want, body)
		}
	}
}

func TestRegistry_AccountSeriesAreSorted(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })
	r.Observe(Pass{Started: base, Healthy: []string{"zulu", "alpha", "mike"}})

	body := scrape(t, r)
	idx := func(name string) int {
		return strings.Index(body, `calendar_bridge_account_healthy{account="`+name+`"}`)
	}
	if !(idx("alpha") < idx("mike") && idx("mike") < idx("zulu")) {
		t.Errorf("account series are not in sorted order:\n%s", body)
	}
}

// No account has ever been seen: the per-account families must be omitted
// entirely rather than emitted as a bare HELP/TYPE with no samples.
func TestRegistry_OmitsAccountFamiliesWhenThereAreNone(t *testing.T) {
	body := scrape(t, New(testBuild(), func() time.Time { return base }))
	if strings.Contains(body, "calendar_bridge_account_healthy") {
		t.Errorf("account_healthy family emitted with no accounts:\n%s", body)
	}
}

func TestHealthz_AlwaysOK(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })
	w := httptest.NewRecorder()
	r.Handler(Options{ReadyMaxAge: time.Minute}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// Liveness must not depend on sync health: an instance that cannot reach
	// Google should keep retrying, not be restarted by its orchestrator.
	if w.Code != http.StatusOK {
		t.Errorf("/healthz = %d with no successful sync, want 200", w.Code)
	}
}

func TestReadyz(t *testing.T) {
	now := base
	clock := func() time.Time { return now }

	t.Run("not ready before the first successful pass", func(t *testing.T) {
		r := New(testBuild(), clock)
		if got := readyzCode(r, 10*time.Minute); got != http.StatusServiceUnavailable {
			t.Errorf("/readyz = %d, want 503", got)
		}
	})

	t.Run("ready after a recent success", func(t *testing.T) {
		r := New(testBuild(), clock)
		r.Observe(Pass{Started: now.Add(-time.Minute), Healthy: []string{"a", "b"}})
		if got := readyzCode(r, 10*time.Minute); got != http.StatusOK {
			t.Errorf("/readyz = %d, want 200", got)
		}
	})

	t.Run("not ready once the last success goes stale", func(t *testing.T) {
		r := New(testBuild(), clock)
		r.Observe(Pass{Started: now.Add(-30 * time.Minute), Healthy: []string{"a", "b"}})
		if got := readyzCode(r, 10*time.Minute); got != http.StatusServiceUnavailable {
			t.Errorf("/readyz = %d for a 30-minute-old success against a 10-minute threshold, want 503", got)
		}
	})

	t.Run("a failed pass does not refresh readiness", func(t *testing.T) {
		r := New(testBuild(), clock)
		r.Observe(Pass{Started: now.Add(-30 * time.Minute), Healthy: []string{"a", "b"}})
		r.Observe(Pass{Started: now, Failed: []string{"a"}, Err: errors.New("boom")})
		if got := readyzCode(r, 10*time.Minute); got != http.StatusServiceUnavailable {
			t.Errorf("/readyz = %d, want 503 — a failed pass must not count as readiness", got)
		}
	})

	t.Run("threshold of zero disables the staleness check", func(t *testing.T) {
		r := New(testBuild(), clock)
		if got := readyzCode(r, 0); got != http.StatusOK {
			t.Errorf("/readyz = %d with the check disabled, want 200", got)
		}
	})
}

func readyzCode(r *Registry, maxAge time.Duration) int {
	w := httptest.NewRecorder()
	r.Handler(Options{ReadyMaxAge: maxAge}).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return w.Code
}

func TestHandler_RejectsNonGET(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })
	h := r.Handler(Options{})
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code == http.StatusOK {
			t.Errorf("POST %s returned 200; the surface is read-only", path)
		}
	}
}

func TestRegistry_ConcurrentObserveAndScrapeAreSafe(t *testing.T) {
	r := New(testBuild(), func() time.Time { return base })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 300 {
			r.Observe(Pass{
				Started: base.Add(time.Duration(i) * time.Second),
				Created: 1, Healthy: []string{"personal", "work-acme"},
			})
		}
	}()
	for range 300 {
		_ = scrape(t, r)
	}
	<-done
}

// assertValidExposition applies the structural rules a Prometheus scraper
// enforces: every sample line is "name{labels} value", every value parses, and
// every metric family has a HELP and TYPE line before its samples.
func assertValidExposition(t *testing.T, body string) {
	t.Helper()

	sampleRe := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})? (\S+)$`)
	declared := map[string]bool{}

	for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 3 {
				t.Errorf("line %d: malformed comment %q", i+1, line)
				continue
			}
			declared[fields[2]] = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		m := sampleRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("line %d is not a valid sample: %q", i+1, line)
			continue
		}
		if _, err := strconv.ParseFloat(m[3], 64); err != nil {
			t.Errorf("line %d: value %q does not parse as a float", i+1, m[3])
		}
		// Histogram sample names carry a suffix over the declared family name.
		name := m[1]
		if !declared[name] {
			trimmed := name
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				trimmed = strings.TrimSuffix(trimmed, suffix)
			}
			if !declared[trimmed] {
				t.Errorf("line %d: series %q has no HELP/TYPE declaration", i+1, name)
			}
		}
	}
}
