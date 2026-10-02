package miniapp

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ceraa/harness/event"
)

// pollBody is the shape handlePoll writes. It is repeated here rather than
// shared with the handler so a change to the wire format has to be made twice
// before a test stops noticing it.
type pollBody struct {
	Seq    int64         `json:"seq"`
	Stale  bool          `json:"stale"`
	Events []loggedEvent `json:"events"`
}

func readPoll(t *testing.T, server *Server, initData, query string) pollBody {
	t.Helper()
	rec := do(t, server.Handler(), http.MethodGet, "/api/events"+query, initData)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll %q = %d, want 200 (%s)", query, rec.Code, rec.Body.String())
	}
	var body pollBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("poll %q body = %q: %v", query, rec.Body.String(), err)
	}
	return body
}

// waitForLog covers the one genuinely asynchronous step: the subscriber fills
// the buffer on its own clock, and a test that publishes and then reads would
// be racing it rather than checking anything.
func waitForLog(t *testing.T, server *Server, seq int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for server.log.head() < seq {
		if time.Now().After(deadline) {
			t.Fatalf("log head = %d, want >= %d", server.log.head(), seq)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPollAttachesAtTheHead is the first request a reader that has just
// arrived makes: it wants what happens next, not a replay of what it never
// saw, so the answer carries a cursor and nothing else.
func TestPollAttachesAtTheHead(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)
	server.ensureLog()

	bus.Publish(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "x"}})
	waitForLog(t, server, 1)

	body := readPoll(t, server, initData, "?wait=0")
	if body.Seq != 1 {
		t.Fatalf("seq = %d, want 1", body.Seq)
	}
	if len(body.Events) != 0 {
		t.Fatalf("events = %d, want 0: a new reader attaches at the head", len(body.Events))
	}
}

// TestPollReturnsWhatFollowsTheCursor is the ordinary batch: ask from a number
// already held, get everything after it, and get the next number to ask from.
func TestPollReturnsWhatFollowsTheCursor(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)
	server.ensureLog()

	bus.Publish(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "a"}})
	bus.Publish(event.Event{Type: event.TypeComplete, SessionID: WebSessionID, Data: map[string]any{"text": "done"}})
	waitForLog(t, server, 2)

	body := readPoll(t, server, initData, "?wait=0&after=0")
	if body.Seq != 2 {
		t.Fatalf("seq = %d, want 2", body.Seq)
	}
	if len(body.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(body.Events))
	}
	if body.Events[0].Seq != 1 || body.Events[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d want 1,2", body.Events[0].Seq, body.Events[1].Seq)
	}
	if body.Events[1].Event.Type != event.TypeComplete {
		t.Fatalf("last type = %s, want complete", body.Events[1].Event.Type)
	}
}

// TestPollTimesOutWithoutEvents keeps an idle session to one request. The
// cursor must come back unchanged, because a reader that slid forward here
// would skip everything published while it was not looking.
func TestPollTimesOutWithoutEvents(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)

	body := readPoll(t, server, initData, "?wait=50")
	if body.Seq != 0 {
		t.Fatalf("seq = %d, want 0 after a quiet wait", body.Seq)
	}
	if len(body.Events) != 0 {
		t.Fatalf("events = %d, want 0", len(body.Events))
	}
}

// TestPollWaitsForSomethingNew is the half that makes batching worth using:
// a quiet session does not spin, and an event published during the wait still
// reaches the reader on the same request rather than the next one.
func TestPollWaitsForSomethingNew(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	live := httptest.NewServer(server.Handler())
	defer live.Close()

	type answer struct {
		body pollBody
		code int
		took time.Duration
	}
	done := make(chan answer, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, live.URL+"/api/events?wait=8000", nil)
		if err != nil {
			done <- answer{code: -1}
			return
		}
		req.Header.Set("Authorization", "tma "+initData)
		started := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- answer{code: -1}
			return
		}
		defer resp.Body.Close()
		var body pollBody
		_ = json.NewDecoder(resp.Body).Decode(&body)
		done <- answer{body: body, code: resp.StatusCode, took: time.Since(started)}
	}()

	time.Sleep(150 * time.Millisecond)
	bus.Publish(event.Event{Type: event.TypePermission, SessionID: WebSessionID, Data: map[string]any{"tool": "bash"}})

	got := <-done
	if got.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.code)
	}
	if len(got.body.Events) != 1 {
		t.Fatalf("events = %d, want the one published mid-wait", len(got.body.Events))
	}
	if got.body.Events[0].Event.Type != event.TypePermission {
		t.Fatalf("type = %s, want permission", got.body.Events[0].Event.Type)
	}
	if got.took < 150*time.Millisecond || got.took > 5*time.Second {
		t.Fatalf("took %v, want it to return when the event arrived rather than at the cap", got.took)
	}
}

// TestPollRefusesABadCursor keeps a malformed request from being answered as
// if it meant the head, which would hand a reader events it has already shown.
func TestPollRefusesABadCursor(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)

	rec := do(t, server.Handler(), http.MethodGet, "/api/events?wait=0&after=abc", initData)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestPollReportsAGapRatherThanSilentlySkipping is the failure that would
// otherwise be invisible: the buffer has aged past the reader's cursor, and
// handing over what still fits would leave the page rendering an answer with
// a hole in it and no way to know one is there.
func TestPollReportsAGapRatherThanSilentlySkipping(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)
	server.ensureLog()

	total := int64(maxEventLog + 7)
	for i := int64(0); i < total; i++ {
		server.log.append(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "x"}})
	}

	body := readPoll(t, server, initData, "?wait=0&after=0")
	if !body.Stale {
		t.Fatal("stale = false, want true: the buffer no longer reaches cursor 0")
	}
	if len(body.Events) != 0 {
		t.Fatalf("events = %d, want 0: a gap is reported, not papered over", len(body.Events))
	}
	if body.Seq != total {
		t.Fatalf("seq = %d, want %d: the reader is moved to the head so it does not ask again", body.Seq, total)
	}
}

// TestTheLogKeepsOnlyTheRecentEvents is the bound itself.
func TestTheLogKeepsOnlyTheRecentEvents(t *testing.T) {
	log := newEventLog()
	total := maxEventLog * 2
	for i := 0; i < total; i++ {
		log.append(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "x"}})
	}
	if len(log.items) != maxEventLog {
		t.Fatalf("buffered = %d, want %d", len(log.items), maxEventLog)
	}
	if log.head() != int64(total) {
		t.Fatalf("head = %d, want %d: the number keeps counting after the buffer trims", log.head(), total)
	}
	if _, stale, _ := log.since(1); !stale {
		t.Fatal("stale = false for a cursor the buffer has dropped")
	}
	if _, stale, _ := log.since(int64(total)); stale {
		t.Fatal("stale = true at the head, where nothing can be missing")
	}
}

// TestStreamGreetsTheReaderIs the transport probe: the page measures whether
// a response reaches it as it is produced by whether this arrives at all, so
// it has to be the first thing written after the banner.
func TestStreamGreetsTheReader(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)

	live := httptest.NewServer(server.Handler())
	defer live.Close()

	req, err := http.NewRequest(http.MethodGet, live.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "tma "+initData)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var opening string
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(opening, `"seq":`) {
		if time.Now().After(deadline) {
			t.Fatalf("opening = %q, want a hello and the cursor it starts from", opening)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading opening: %v", err)
		}
		opening += line
	}
	if !strings.Contains(opening, "event: hello") {
		t.Fatalf("opening = %q, want a hello before the cursor", opening)
	}
}

// TestStreamResumesFromTheCursor is why the buffer exists at all. The bus
// drops what nobody is listening to, so a reader that reconnects has missed
// the whole gap and can only be caught up from what was kept.
func TestStreamResumesFromTheCursor(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	t.Cleanup(func() { _ = server.Close() })
	initData := initDataFor(t, testToken, testUser, 0)
	server.ensureLog()
	server.log.append(event.Event{Type: event.TypePermission, SessionID: WebSessionID, Data: map[string]any{"tool": "bash"}})

	live := httptest.NewServer(server.Handler())
	defer live.Close()

	req, err := http.NewRequest(http.MethodGet, live.URL+"/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "tma "+initData)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var got string
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(got, `"tool":"bash"`) {
		if time.Now().After(deadline) {
			t.Fatalf("replay = %q, want the missed permission event", got)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading replay: %v", err)
		}
		got += line
	}
	if !strings.Contains(got, "id: 1") {
		t.Fatalf("replay = %q, want the sequence it carries", got)
	}
	if !strings.Contains(got, "event: permission") {
		t.Fatalf("replay = %q, want the missed permission event", got)
	}
}

// TestStopLogEndsTheSubscriber keeps shutdown from leaving one behind. A
// process that closes the surface and then keeps publishing would otherwise
// keep filling a buffer nobody can read.
func TestStopLogEndsTheSubscriber(t *testing.T) {
	server, _, bus := newFixture(t, testUser)
	server.ensureLog()
	bus.Publish(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "x"}})
	waitForLog(t, server, 1)

	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	bus.Publish(event.Event{Type: event.TypeContent, SessionID: WebSessionID, Data: map[string]any{"delta": "y"}})
	time.Sleep(60 * time.Millisecond)

	if got := server.log.head(); got != 1 {
		t.Fatalf("head = %d after shutdown, want 1: the subscriber is gone", got)
	}
}
