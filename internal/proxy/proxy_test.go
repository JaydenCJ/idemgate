// Integration tests for the gate, run against real in-process loopback
// backends (httptest). Every scenario from the decision table in
// proxy.go is exercised end to end: execute, replay, 409, 422, 400, 413,
// 502, expiry, restart durability and proxy header hygiene. Concurrency
// is orchestrated with channels — no sleeps, no timing assumptions.
package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JaydenCJ/idemgate/internal/config"
	"github.com/JaydenCJ/idemgate/internal/store"
)

// backend wraps an httptest server with an execution counter and a copy
// of the headers the last request arrived with.
type backend struct {
	srv  *httptest.Server
	hits atomic.Int64

	mu         sync.Mutex
	lastHeader http.Header
	lastPath   string
}

// newBackend starts a loopback backend. handler == nil means the default
// charge-creating handler: every execution mints a fresh ch_<n>, which is
// exactly the non-idempotent behavior the gate exists to contain.
func newBackend(t *testing.T, handler http.HandlerFunc) *backend {
	t.Helper()
	b := &backend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := b.hits.Add(1)
		b.mu.Lock()
		b.lastHeader = r.Header.Clone()
		b.lastPath = r.URL.Path
		b.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", fmt.Sprintf("/charges/ch_%d", n))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"ch_%d"}`, n)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// newGate builds a gate over the given upstream with a controllable
// clock. mutate tweaks the config before the store is created.
func newGate(t *testing.T, upstream string, mutate func(*config.Config)) (*Gate, *store.Store, *time.Time) {
	t.Helper()
	cfg := config.Default()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("bad upstream %q: %v", upstream, err)
	}
	cfg.Upstream = u
	if mutate != nil {
		mutate(cfg)
	}
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	st := store.New(t.TempDir(), cfg.TTL, cfg.LeaseTimeout)
	st.Now = func() time.Time { return now }
	if err := st.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return New(cfg, st), st, &now
}

// do drives one request through the gate in-process.
func do(t *testing.T, g http.Handler, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func keyed(key string) map[string]string {
	return map[string]string{"Idempotency-Key": key}
}

const chargeURL = "http://pay.example.test/charges"

func TestUngatedAndKeylessRequestsPassThrough(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, nil)

	// GET is a safe method: even with a key it is proxied, never gated.
	for i := 0; i < 2; i++ {
		w := do(t, g, "GET", chargeURL, "", keyed("k-get"))
		if w.Header().Get(ReplayedHeader) != "" {
			t.Fatal("GET response marked as replayed")
		}
	}
	// POST without a key is proxied untouched by default.
	do(t, g, "POST", chargeURL, `{"amount":1}`, nil)
	do(t, g, "POST", chargeURL, `{"amount":1}`, nil)
	if got := b.hits.Load(); got != 4 {
		t.Fatalf("expected 4 backend executions, got %d", got)
	}
}

func TestKeyValidationErrors(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, func(c *config.Config) { c.RequireKey = true })

	w := do(t, g, "POST", chargeURL, `{"amount":1}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("keyless POST under --require-key: got %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("gate error is not problem+json: %q", ct)
	}

	w = do(t, g, "POST", chargeURL, `{"amount":1}`, keyed(strings.Repeat("k", 300)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("300-byte key: got %d, want 400", w.Code)
	}
	if b.hits.Load() != 0 {
		t.Fatal("invalid requests reached the backend")
	}
}

func TestFirstExecutionForwardsAndStores(t *testing.T) {
	b := newBackend(t, nil)
	g, st, _ := newGate(t, b.srv.URL, nil)

	w := do(t, g, "POST", chargeURL, `{"amount":1999}`, keyed("order-42"))
	if w.Code != http.StatusCreated || w.Body.String() != `{"id":"ch_1"}` {
		t.Fatalf("unexpected first response: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Location") != "/charges/ch_1" {
		t.Fatalf("Location not forwarded: %q", w.Header().Get("Location"))
	}
	if w.Header().Get(ReplayedHeader) != "" {
		t.Fatal("first execution marked as replayed")
	}
	rec, err := st.Get("order-42")
	if err != nil || rec == nil || rec.Status != 201 {
		t.Fatalf("response not stored: (%+v, %v)", rec, err)
	}
}

func TestDuplicateReplaysWithoutTouchingBackend(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, nil)

	first := do(t, g, "POST", chargeURL, `{"amount":1999}`, keyed("order-42"))
	second := do(t, g, "POST", chargeURL, `{"amount":1999}`, keyed("order-42"))

	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("replay differs: %d %q vs %d %q", second.Code, second.Body.String(), first.Code, first.Body.String())
	}
	if second.Header().Get(ReplayedHeader) != "true" {
		t.Fatal("replay not marked with Idempotency-Replayed: true")
	}
	if second.Header().Get("Location") != "/charges/ch_1" {
		t.Fatalf("replay lost the Location header: %q", second.Header().Get("Location"))
	}
	if got := b.hits.Load(); got != 1 {
		t.Fatalf("backend executed %d times, want exactly 1", got)
	}
}

func TestQuotedAndBareKeyShareARecord(t *testing.T) {
	// The IETF draft quotes the key; Stripe-style clients send it bare.
	// Both spellings must deduplicate against each other.
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, nil)

	do(t, g, "POST", chargeURL, `{"amount":1}`, keyed(`"order-42"`))
	w := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("order-42"))
	if w.Header().Get(ReplayedHeader) != "true" || b.hits.Load() != 1 {
		t.Fatalf("quoted and bare keys did not share a record (hits=%d)", b.hits.Load())
	}
}

func TestKeyReuseWithDifferentRequestIs422(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, nil)
	do(t, g, "POST", chargeURL, `{"amount":1999}`, keyed("order-42"))

	// Same key, new body — and same key, new path. Both are client bugs
	// and must be refused, not silently replayed or re-executed.
	for _, tc := range []struct{ target, body string }{
		{chargeURL, `{"amount":2500}`},
		{"http://pay.example.test/refunds", `{"amount":1999}`},
	} {
		w := do(t, g, "POST", tc.target, tc.body, keyed("order-42"))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("reuse %q %q: got %d, want 422", tc.target, tc.body, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("422 is not problem+json: %q", ct)
		}
	}
	// The original record is untouched and still replayable.
	w := do(t, g, "POST", chargeURL, `{"amount":1999}`, keyed("order-42"))
	if w.Header().Get(ReplayedHeader) != "true" || b.hits.Load() != 1 {
		t.Fatalf("original record damaged by reuse attempts (hits=%d)", b.hits.Load())
	}
}

func TestDifferentKeysExecuteIndependently(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, nil)

	w1 := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	w2 := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k2"))
	if w1.Body.String() == w2.Body.String() {
		t.Fatalf("distinct keys produced the same charge: %q", w1.Body.String())
	}
	if b.hits.Load() != 2 {
		t.Fatalf("expected 2 executions, got %d", b.hits.Load())
	}
}

func TestEmptyBodyRequestsDeduplicate(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, nil)

	do(t, g, "POST", "http://pay.example.test/captures/ch_1", "", keyed("cap-1"))
	w := do(t, g, "POST", "http://pay.example.test/captures/ch_1", "", keyed("cap-1"))
	if w.Header().Get(ReplayedHeader) != "true" || b.hits.Load() != 1 {
		t.Fatalf("empty-body request not deduplicated (hits=%d)", b.hits.Load())
	}
}

func Test4xxResponsesAreStoredAndReplayed(t *testing.T) {
	// A definitive client-error answer must stay definitive: retrying
	// a card decline should not charge the card a second time.
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":"card_declined"}`)
	})
	g, _, _ := newGate(t, b.srv.URL, nil)

	do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	w := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w.Code != http.StatusPaymentRequired || w.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("402 not replayed: %d %v", w.Code, w.Header())
	}
	if b.hits.Load() != 1 {
		t.Fatalf("4xx retry reached the backend (hits=%d)", b.hits.Load())
	}
}

func Test5xxResponsesAreNotStored(t *testing.T) {
	// Server errors stay retryable: the next attempt with the same key
	// must re-execute, not replay the outage.
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	})
	g, _, _ := newGate(t, b.srv.URL, nil)

	w1 := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	w2 := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w1.Code != 503 || w2.Code != 503 {
		t.Fatalf("5xx not forwarded: %d %d", w1.Code, w2.Code)
	}
	if w2.Header().Get(ReplayedHeader) != "" {
		t.Fatal("5xx was replayed")
	}
	if b.hits.Load() != 2 {
		t.Fatalf("expected 2 executions, got %d", b.hits.Load())
	}
}

func TestUpstreamFailureReleasesLease(t *testing.T) {
	// Point at a port that answers nothing: grab one, then free it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	g, _, _ := newGate(t, "http://"+dead, nil)

	w1 := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w1.Code != http.StatusBadGateway {
		t.Fatalf("dead upstream: got %d, want 502", w1.Code)
	}
	// The lease must be gone: a retry gets 502 again, never 409.
	w2 := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w2.Code != http.StatusBadGateway {
		t.Fatalf("retry after upstream failure: got %d, want 502", w2.Code)
	}
}

func TestConcurrentDuplicateConflicts(t *testing.T) {
	// The first request parks inside the backend; concurrent arrivals
	// with the same key are answered without waiting: 409 for an exact
	// duplicate, 422 for a conflicting payload.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			close(entered)
			<-release
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"ch_1"}`)
	})
	g, _, _ := newGate(t, b.srv.URL, nil)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	}()
	<-entered

	w := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w.Code != http.StatusConflict {
		t.Fatalf("concurrent duplicate: got %d, want 409", w.Code)
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Fatalf("409 missing Retry-After: %v", w.Header())
	}

	w = do(t, g, "POST", chargeURL, `{"amount":2}`, keyed("k1"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("concurrent conflicting payload: got %d, want 422", w.Code)
	}

	close(release)
	if first := <-firstDone; first.Code != http.StatusCreated {
		t.Fatalf("original request failed: %d", first.Code)
	}
	w = do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w.Header().Get(ReplayedHeader) != "true" {
		t.Fatal("post-completion duplicate not replayed")
	}
	if b.hits.Load() != 1 {
		t.Fatalf("backend executed %d times, want exactly 1", b.hits.Load())
	}
}

func TestRequestBodyCap(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL, func(c *config.Config) { c.MaxRequest = 8 })

	// Exactly at the cap is fine.
	w := do(t, g, "POST", chargeURL, "12345678", keyed("k1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("body at cap rejected: %d", w.Code)
	}
	// One byte over cannot be fingerprinted safely: refuse loudly.
	w = do(t, g, "POST", chargeURL, "123456789", keyed("k2"))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over cap: got %d, want 413", w.Code)
	}
	if b.hits.Load() != 1 {
		t.Fatalf("oversize keyed request reached the backend (hits=%d)", b.hits.Load())
	}
}

func TestOversizeResponseDeliveredButNotStored(t *testing.T) {
	const body = "0123456789abcdef" // 16 bytes
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, body)
	})
	g, _, _ := newGate(t, b.srv.URL, func(c *config.Config) { c.MaxResponse = 8 })

	// The client must still receive every byte...
	w := do(t, g, "POST", chargeURL, `{"a":1}`, keyed("k1"))
	if w.Body.String() != body {
		t.Fatalf("oversize response truncated: %q", w.Body.String())
	}
	// ...but nothing was stored, so a retry re-executes.
	do(t, g, "POST", chargeURL, `{"a":1}`, keyed("k1"))
	if b.hits.Load() != 2 {
		t.Fatalf("oversize response was stored (hits=%d)", b.hits.Load())
	}
}

func TestExpiredRecordReExecutes(t *testing.T) {
	b := newBackend(t, nil)
	g, _, now := newGate(t, b.srv.URL, nil)

	do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	*now = now.Add(25 * time.Hour) // past the 24h TTL

	w := do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w.Header().Get(ReplayedHeader) != "" || b.hits.Load() != 2 {
		t.Fatalf("expired key not re-executed (hits=%d)", b.hits.Load())
	}
	// The fresh execution is stored again.
	w = do(t, g, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w.Header().Get(ReplayedHeader) != "true" || b.hits.Load() != 2 {
		t.Fatalf("re-executed response not stored (hits=%d)", b.hits.Load())
	}
}

func TestRecordsSurviveRestart(t *testing.T) {
	// Dedup must not depend on process memory: a new gate over the same
	// store directory replays what the old one stored.
	b := newBackend(t, nil)
	g1, st1, now := newGate(t, b.srv.URL, nil)
	do(t, g1, "POST", chargeURL, `{"amount":1}`, keyed("k1"))

	st2 := store.New(st1.Dir, st1.TTL, st1.LeaseTimeout)
	st2.Now = func() time.Time { return *now }
	g2 := New(g1.Config, st2)

	w := do(t, g2, "POST", chargeURL, `{"amount":1}`, keyed("k1"))
	if w.Header().Get(ReplayedHeader) != "true" || b.hits.Load() != 1 {
		t.Fatalf("record did not survive the restart (hits=%d)", b.hits.Load())
	}
}

func TestProxyHeaderHygiene(t *testing.T) {
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Keep-Alive", "timeout=5") // hop-by-hop: must not survive
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
	})
	g, _, _ := newGate(t, b.srv.URL, nil)

	first := do(t, g, "POST", chargeURL, `{"a":1}`, map[string]string{
		"Idempotency-Key":     "hygiene-1",
		"Connection":          "X-Hop",
		"X-Hop":               "1",
		"Proxy-Authorization": "Basic secret",
		"X-Forwarded-For":     "203.0.113.9",
	})

	b.mu.Lock()
	seen := b.lastHeader
	b.mu.Unlock()
	if seen.Get("X-Hop") != "" || seen.Get("Proxy-Authorization") != "" {
		t.Fatalf("hop-by-hop request headers leaked upstream: %v", seen)
	}
	if seen.Get("Idempotency-Key") != "hygiene-1" {
		t.Fatal("idempotency key not forwarded to the backend")
	}
	// httptest requests arrive from 192.0.2.1; the existing chain is extended.
	if got := seen.Get("X-Forwarded-For"); got != "203.0.113.9, 192.0.2.1" {
		t.Fatalf("X-Forwarded-For chain wrong: %q", got)
	}
	if seen.Get("X-Forwarded-Host") != "pay.example.test" || seen.Get("X-Forwarded-Proto") != "http" {
		t.Fatalf("forwarding headers wrong: %v", seen)
	}
	if !strings.Contains(seen.Get("Via"), "1.1 idemgate") {
		t.Fatalf("Via missing: %q", seen.Get("Via"))
	}

	replay := do(t, g, "POST", chargeURL, `{"a":1}`, keyed("hygiene-1"))
	for name, w := range map[string]*httptest.ResponseRecorder{"first": first, "replay": replay} {
		if w.Header().Get("Keep-Alive") != "" {
			t.Fatalf("%s response leaked a hop-by-hop header", name)
		}
	}
}

func TestBinaryResponseReplayedByteExact(t *testing.T) {
	// Compressed bodies must round-trip untouched: the gate never
	// decompresses, so stored bytes are the wire bytes.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, `{"id":"ch_1","note":"compressed"}`)
	zw.Close()

	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write(gz.Bytes())
	})
	g, _, _ := newGate(t, b.srv.URL, nil)

	first := do(t, g, "POST", chargeURL, `{"a":1}`, keyed("k1"))
	replay := do(t, g, "POST", chargeURL, `{"a":1}`, keyed("k1"))
	if !bytes.Equal(first.Body.Bytes(), gz.Bytes()) {
		t.Fatal("first pass mangled the compressed body")
	}
	if !bytes.Equal(replay.Body.Bytes(), gz.Bytes()) {
		t.Fatal("replay mangled the compressed body")
	}
	if replay.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("replay lost Content-Encoding")
	}
}

func TestUpstreamPathPrefixIsJoined(t *testing.T) {
	b := newBackend(t, nil)
	g, _, _ := newGate(t, b.srv.URL+"/api/v2", nil)

	do(t, g, "POST", chargeURL, `{"a":1}`, keyed("k1"))
	b.mu.Lock()
	path := b.lastPath
	b.mu.Unlock()
	if path != "/api/v2/charges" {
		t.Fatalf("path prefix not joined: %q", path)
	}
}
