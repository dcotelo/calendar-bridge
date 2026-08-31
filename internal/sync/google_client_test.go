package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// These tests exercise googleCalendarClient — the real Calendar API client —
// against an httptest server speaking the Calendar API's wire format. Every
// other test in this package substitutes a fake at the CalendarClient
// interface, which means pagination, query construction, the conditional-delete
// header and the 404-to-nil mapping were previously untested: they live below
// that seam.
//
// Nothing here contacts Google.

// fakeCalendarAPI is an httptest server implementing just enough of the
// Calendar API v3 surface that googleCalendarClient uses.
type fakeCalendarAPI struct {
	t   *testing.T
	srv *httptest.Server

	// pages, when set, is served in order by successive Events.List calls,
	// with a nextPageToken linking them.
	pages [][]*calendar.Event
	// single is served when pages is empty.
	single []*calendar.Event

	// Recorded request details for assertions.
	listQueries []url.Values
	deleteETags []string
	updateETags []string
	deletePaths []string

	// Behaviour injection.
	getStatus    int // status for Events.Get; 0 means 200
	deleteStatus int // status for Events.Delete; 0 means 204
	listFailures int32
	listStatus   int // status returned while listFailures remains
	// writeFailures / writeStatus do the same for Events.Insert and
	// Events.Update, so the write paths' failure handling is exercised at the
	// wire and not only against synthetic error values.
	writeFailures int32
	writeStatus   int
}

func newFakeCalendarAPI(t *testing.T) *fakeCalendarAPI {
	t.Helper()
	f := &fakeCalendarAPI{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCalendarAPI) serve(w http.ResponseWriter, r *http.Request) {
	// Paths look like /calendars/{calendarID}/events[/{eventID}]
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 3: // list
		f.handleList(w, r)
	case r.Method == http.MethodGet && len(parts) == 4: // get
		f.handleGet(w, parts[3])
	case r.Method == http.MethodDelete && len(parts) == 4:
		f.handleDelete(w, r, parts[3])
	case r.Method == http.MethodPost && len(parts) == 3: // insert
		f.echoEvent(w, r, "inserted-id")
	case r.Method == http.MethodPut && len(parts) == 4: // update
		f.echoEvent(w, r, parts[3])
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeCalendarAPI) handleList(w http.ResponseWriter, r *http.Request) {
	f.listQueries = append(f.listQueries, r.URL.Query())

	if f.listStatus != 0 && atomic.AddInt32(&f.listFailures, -1) >= 0 {
		writeAPIError(w, f.listStatus, "transient")
		return
	}

	res := &calendar.Events{}
	if len(f.pages) > 0 {
		idx := 0
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			if _, err := fmt.Sscanf(tok, "page-%d", &idx); err != nil {
				http.Error(w, "bad page token", http.StatusBadRequest)
				return
			}
		}
		if idx >= len(f.pages) {
			http.Error(w, "page out of range", http.StatusBadRequest)
			return
		}
		res.Items = f.pages[idx]
		if idx+1 < len(f.pages) {
			res.NextPageToken = fmt.Sprintf("page-%d", idx+1)
		}
	} else {
		res.Items = f.single
	}
	writeJSON(w, http.StatusOK, res)
}

func (f *fakeCalendarAPI) handleGet(w http.ResponseWriter, id string) {
	if f.getStatus != 0 {
		writeAPIError(w, f.getStatus, "get failed")
		return
	}
	writeJSON(w, http.StatusOK, &calendar.Event{Id: id, Etag: `"etag-` + id + `"`})
}

func (f *fakeCalendarAPI) handleDelete(w http.ResponseWriter, r *http.Request, id string) {
	f.deleteETags = append(f.deleteETags, r.Header.Get("If-Match"))
	f.deletePaths = append(f.deletePaths, id)
	if f.deleteStatus != 0 {
		writeAPIError(w, f.deleteStatus, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeCalendarAPI) echoEvent(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == http.MethodPut {
		f.updateETags = append(f.updateETags, r.Header.Get("If-Match"))
	}
	if f.writeStatus != 0 && atomic.AddInt32(&f.writeFailures, -1) >= 0 {
		writeAPIError(w, f.writeStatus, "write failed")
		return
	}
	var ev calendar.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	ev.Id = id
	writeJSON(w, http.StatusOK, &ev)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}

// client builds a googleCalendarClient pointed at the fake server.
func (f *fakeCalendarAPI) client() CalendarClient {
	f.t.Helper()
	svc, err := calendar.NewService(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(f.srv.URL),
		option.WithHTTPClient(f.srv.Client()),
	)
	if err != nil {
		f.t.Fatalf("building calendar service: %v", err)
	}
	return NewGoogleCalendarClient(svc)
}

func evAt(id string, start time.Time) *calendar.Event {
	return &calendar.Event{
		Id:    id,
		Etag:  `"etag-` + id + `"`,
		Start: &calendar.EventDateTime{DateTime: start.Format(time.RFC3339)},
		End:   &calendar.EventDateTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	}
}

// ListEvents must follow nextPageToken to completion. A calendar with more
// than one page of events is entirely normal, and stopping at the first page
// would make every event after it look deleted — which GC would act on.
func TestGoogleClient_ListEventsFollowsPagination(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.pages = [][]*calendar.Event{
		{evAt("a", baseTime), evAt("b", baseTime.Add(time.Hour))},
		{evAt("c", baseTime.Add(2*time.Hour))},
		{evAt("d", baseTime.Add(3*time.Hour)), evAt("e", baseTime.Add(4*time.Hour))},
	}

	got, err := api.client().ListEvents(context.Background(), "primary", baseTime, baseTime.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d events across 3 pages, want 5 — pagination stopped early", len(got))
	}
	var ids []string
	for _, ev := range got {
		ids = append(ids, ev.Id)
	}
	if strings.Join(ids, ",") != "a,b,c,d,e" {
		t.Errorf("ids = %v, want a,b,c,d,e in order", ids)
	}
	if len(api.listQueries) != 3 {
		t.Errorf("made %d list calls, want 3", len(api.listQueries))
	}
}

// The request must expand recurring events into instances and bound the window,
// or the engine reasons about the wrong set of events entirely.
func TestGoogleClient_ListEventsQueryParameters(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.single = []*calendar.Event{evAt("a", baseTime)}

	tMin := baseTime.Add(-24 * time.Hour)
	tMax := baseTime.Add(30 * 24 * time.Hour)
	if _, err := api.client().ListEvents(context.Background(), "primary", tMin, tMax); err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(api.listQueries) != 1 {
		t.Fatalf("want 1 list call, got %d", len(api.listQueries))
	}
	q := api.listQueries[0]

	if q.Get("singleEvents") != "true" {
		t.Error("singleEvents is not true; recurring events would come back as a single master event " +
			"rather than the instances the engine needs to block time for")
	}
	if got := q.Get("timeMin"); got != tMin.Format(time.RFC3339) {
		t.Errorf("timeMin = %q, want %q", got, tMin.Format(time.RFC3339))
	}
	if got := q.Get("timeMax"); got != tMax.Format(time.RFC3339) {
		t.Errorf("timeMax = %q, want %q", got, tMax.Format(time.RFC3339))
	}
	if q.Get("maxResults") == "" {
		t.Error("maxResults is unset; the default page size would make pagination far chattier")
	}
}

// FindBlockBySource must filter on both private extended properties, and must
// not return an event that merely matches those properties without carrying
// the owner tag.
func TestGoogleClient_FindBlockBySource(t *testing.T) {
	owned := evAt("blk-1", baseTime)
	owned.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{
		ownerKey:          ownerValue,
		sourceAccountKey:  "personal",
		sourceCalendarKey: "primary",
		sourceEventKey:    "evt-1",
	}}
	impostor := evAt("real-1", baseTime)
	impostor.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{
		// Same source properties, but no owner tag: a real user event.
		sourceAccountKey: "personal",
		sourceEventKey:   "evt-1",
	}}

	t.Run("returns the owned block", func(t *testing.T) {
		api := newFakeCalendarAPI(t)
		api.single = []*calendar.Event{impostor, owned}

		got, err := api.client().FindBlockBySource(context.Background(), "primary", "personal", "evt-1")
		if err != nil {
			t.Fatalf("FindBlockBySource: %v", err)
		}
		if got == nil || got.Id != "blk-1" {
			t.Fatalf("got %v, want the owned block blk-1", got)
		}

		q := api.listQueries[0]
		props := q["privateExtendedProperty"]
		if len(props) != 2 {
			t.Fatalf("privateExtendedProperty = %v, want both the account and event filters", props)
		}
		joined := strings.Join(props, " ")
		if !strings.Contains(joined, sourceAccountKey+"=personal") || !strings.Contains(joined, sourceEventKey+"=evt-1") {
			t.Errorf("privateExtendedProperty = %v, want the source account and event filters", props)
		}
	})

	t.Run("never returns an untagged impostor", func(t *testing.T) {
		api := newFakeCalendarAPI(t)
		api.single = []*calendar.Event{impostor}

		got, err := api.client().FindBlockBySource(context.Background(), "primary", "personal", "evt-1")
		if err != nil {
			t.Fatalf("FindBlockBySource: %v", err)
		}
		if got != nil {
			t.Fatalf("returned %q for an event with matching properties but no owner tag; "+
				"the caller would then overwrite a real user event", got.Id)
		}
	})

	t.Run("skips a cancelled block", func(t *testing.T) {
		cancelled := *owned
		cancelled.Status = "cancelled"
		api := newFakeCalendarAPI(t)
		api.single = []*calendar.Event{&cancelled}

		got, err := api.client().FindBlockBySource(context.Background(), "primary", "personal", "evt-1")
		if err != nil {
			t.Fatalf("FindBlockBySource: %v", err)
		}
		if got != nil {
			t.Errorf("returned a cancelled block (%q)", got.Id)
		}
	})
}

// A 404 from Events.Get means "no such event", which is a legitimate state
// (the block was already deleted), not an error to propagate.
func TestGoogleClient_GetEventMapsNotFoundToNil(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.getStatus = http.StatusNotFound

	got, err := api.client().GetEvent(context.Background(), "primary", "gone")
	if err != nil {
		t.Fatalf("GetEvent on a missing event = %v, want nil error", err)
	}
	if got != nil {
		t.Errorf("GetEvent returned %v, want nil", got)
	}
}

func TestGoogleClient_GetEventPropagatesOtherErrors(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.getStatus = http.StatusForbidden

	_, err := api.client().GetEvent(context.Background(), "primary", "blk-1")
	if err == nil {
		t.Fatal("want an error for a 403")
	}
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusForbidden {
		t.Errorf("err = %v, want a googleapi.Error with code 403", err)
	}
}

// The conditional delete is what stops GC removing an event that changed
// between the list and the delete. If the If-Match header were dropped, that
// protection would silently vanish.
func TestGoogleClient_DeleteEventSendsIfMatch(t *testing.T) {
	api := newFakeCalendarAPI(t)

	if err := api.client().DeleteEvent(context.Background(), "primary", "blk-1", `"etag-blk-1"`); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if len(api.deleteETags) != 1 {
		t.Fatalf("want 1 delete, got %d", len(api.deleteETags))
	}
	if got := api.deleteETags[0]; got != `"etag-blk-1"` {
		t.Errorf("If-Match = %q, want the ETag to be sent; without it the delete is unconditional", got)
	}
	if api.deletePaths[0] != "blk-1" {
		t.Errorf("deleted %q, want blk-1", api.deletePaths[0])
	}
}

func TestGoogleClient_DeleteEventOmitsIfMatchWhenETagIsEmpty(t *testing.T) {
	api := newFakeCalendarAPI(t)

	if err := api.client().DeleteEvent(context.Background(), "primary", "blk-1", ""); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if got := api.deleteETags[0]; got != "" {
		t.Errorf("If-Match = %q, want it omitted when no ETag is known", got)
	}
}

func TestGoogleClient_DeleteEventSurfacesPreconditionFailure(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.deleteStatus = http.StatusPreconditionFailed

	err := api.client().DeleteEvent(context.Background(), "primary", "blk-1", `"stale"`)
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusPreconditionFailed {
		t.Fatalf("err = %v, want a 412 so the caller re-reads instead of deleting a changed event", err)
	}
}

func TestGoogleClient_InsertAndUpdateRoundTripTheEvent(t *testing.T) {
	api := newFakeCalendarAPI(t)
	c := api.client()

	block := &calendar.Event{
		Summary:      "Busy (calendar-bridge)",
		Transparency: "opaque",
		Visibility:   "private",
		Start:        &calendar.EventDateTime{DateTime: baseTime.Format(time.RFC3339)},
		End:          &calendar.EventDateTime{DateTime: baseTime.Add(time.Hour).Format(time.RFC3339)},
		ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
			ownerKey:          ownerValue,
			sourceAccountKey:  "personal",
			sourceCalendarKey: "primary",
			sourceEventKey:    "evt-1",
		}},
	}

	created, err := c.InsertEvent(context.Background(), "primary", block)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if created.Id != "inserted-id" {
		t.Errorf("created.Id = %q", created.Id)
	}
	// The ownership tag must survive serialisation, or the block would be
	// indistinguishable from a real event on the next pass.
	if !isOwnedBlock(created) {
		t.Error("the ownership tag did not survive the insert round trip")
	}
	if created.Transparency != "opaque" || created.Visibility != "private" {
		t.Errorf("transparency/visibility = %q/%q, want opaque/private", created.Transparency, created.Visibility)
	}

	created.Start.DateTime = baseTime.Add(2 * time.Hour).Format(time.RFC3339)
	updated, err := c.UpdateEvent(context.Background(), "primary", created.Id, created, `"etag-abc"`)
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if !isOwnedBlock(updated) {
		t.Error("the ownership tag did not survive the update round trip")
	}
	if updated.Summary != "Busy (calendar-bridge)" {
		t.Errorf("summary = %q, want it preserved across a full-replace update", updated.Summary)
	}
	// The conditional update is what makes the ownership check and the write
	// atomic. Verified at the wire, not just at the interface.
	if len(api.updateETags) != 1 || api.updateETags[0] != `"etag-abc"` {
		t.Errorf("If-Match on update = %v, want it sent; without it an event that lost its owner "+
			"tag between the read and the write would be overwritten", api.updateETags)
	}
}

func TestGoogleClient_UpdateEventOmitsIfMatchWhenETagIsEmpty(t *testing.T) {
	api := newFakeCalendarAPI(t)
	c := api.client()

	if _, err := c.UpdateEvent(context.Background(), "primary", "blk-1",
		&calendar.Event{Summary: "Busy"}, ""); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if len(api.updateETags) != 1 || api.updateETags[0] != "" {
		t.Errorf("If-Match = %v, want it omitted when no ETag is known", api.updateETags)
	}
}

// The retry decorator must actually retry a 429 against a real HTTP transport,
// not just against a fake that returns a synthetic error value.
func TestGoogleClient_RetriesRateLimitingOverHTTP(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.single = []*calendar.Event{evAt("a", baseTime)}
	api.listStatus = http.StatusTooManyRequests
	atomic.StoreInt32(&api.listFailures, 2) // fail twice, then succeed

	c := NewRetryingClient(api.client(),
		RetryPolicy{MaxAttempts: 4, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond},
		newTestLogger(), "test")

	got, err := c.ListEvents(context.Background(), "primary", baseTime, baseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEvents through the retrying client: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d events, want 1", len(got))
	}
	if len(api.listQueries) != 3 {
		t.Errorf("made %d list calls, want 3 (two 429s then a success)", len(api.listQueries))
	}
}

// A 403 is not transient: retrying it wastes quota and delays surfacing a
// problem the operator has to fix (revoked access, disabled API).
func TestGoogleClient_DoesNotRetryForbidden(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.listStatus = http.StatusForbidden
	atomic.StoreInt32(&api.listFailures, 100)

	c := NewRetryingClient(api.client(),
		RetryPolicy{MaxAttempts: 4, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		newTestLogger(), "test")

	if _, err := c.ListEvents(context.Background(), "primary", baseTime, baseTime.Add(time.Hour)); err == nil {
		t.Fatal("want an error for a 403")
	}
	if len(api.listQueries) != 1 {
		t.Errorf("made %d list calls for a 403, want exactly 1 — 4xx other than 429 must not be retried", len(api.listQueries))
	}
}

// A cancelled context must stop the client promptly rather than running the
// full retry budget.
func TestGoogleClient_HonoursContextCancellation(t *testing.T) {
	api := newFakeCalendarAPI(t)
	api.single = []*calendar.Event{evAt("a", baseTime)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := api.client().ListEvents(ctx, "primary", baseTime, baseTime.Add(time.Hour))
	if err == nil {
		t.Fatal("want an error for a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// The retry policy must behave the same on WRITES as it does on reads, and the
// distinction matters more here: retrying a write that already succeeded is how
// duplicates appear, so the classification has to be right at the wire and not
// only against synthetic error values.
func TestGoogleClient_WriteRetryClassification(t *testing.T) {
	block := func() *calendar.Event {
		return &calendar.Event{
			Summary: "Busy (calendar-bridge)",
			Start:   &calendar.EventDateTime{DateTime: baseTime.Format(time.RFC3339)},
			End:     &calendar.EventDateTime{DateTime: baseTime.Add(time.Hour).Format(time.RFC3339)},
			ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
				ownerKey:          ownerValue,
				sourceAccountKey:  "personal",
				sourceCalendarKey: "primary",
				sourceEventKey:    "evt-1",
			}},
		}
	}

	cases := []struct {
		name         string
		status       int
		failures     int32
		wantErr      bool
		wantAttempts int
	}{
		{"500 is retried and then succeeds", http.StatusInternalServerError, 2, false, 3},
		{"429 is retried and then succeeds", http.StatusTooManyRequests, 1, false, 2},
		// A 4xx other than 429 will not succeed on retry and usually means a
		// config or auth problem the operator must fix; retrying burns quota
		// and delays the signal.
		{"403 is not retried", http.StatusForbidden, 100, true, 1},
		{"400 is not retried", http.StatusBadRequest, 100, true, 1},
	}

	// Both write verbs, because a retry regression could easily land on one and
	// not the other: insert and update take different paths through the
	// provider and the bridge.
	verbs := map[string]func(CalendarClient) error{
		"InsertEvent": func(c CalendarClient) error {
			_, err := c.InsertEvent(context.Background(), "primary", block())
			return err
		},
		"UpdateEvent": func(c CalendarClient) error {
			_, err := c.UpdateEvent(context.Background(), "primary", "blk-1", block(), `"etag-1"`)
			return err
		},
	}

	for verbName, call := range verbs {
		for _, tc := range cases {
			t.Run(verbName+"/"+tc.name, func(t *testing.T) {
				api := newFakeCalendarAPI(t)
				api.writeStatus = tc.status
				atomic.StoreInt32(&api.writeFailures, tc.failures)

				c := NewRetryingClient(api.client(),
					RetryPolicy{MaxAttempts: 4, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond},
					newTestLogger(), "test")

				err := call(c)
				if tc.wantErr && err == nil {
					t.Fatalf("%s succeeded; want an error for %d", verbName, tc.status)
				}
				if !tc.wantErr && err != nil {
					t.Fatalf("%s: %v", verbName, err)
				}
				// writeFailures counts down from the seeded value, so the
				// number of attempts is derivable from what remains.
				attempts := int(tc.failures - atomic.LoadInt32(&api.writeFailures))
				if attempts != tc.wantAttempts {
					t.Errorf("made %d %s attempts, want %d", attempts, verbName, tc.wantAttempts)
				}
			})
		}
	}
}
