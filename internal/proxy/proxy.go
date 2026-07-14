// Package proxy implements the idempotency gate: an http.Handler that
// forwards traffic to one upstream and deduplicates requests that carry
// an Idempotency-Key.
//
// Decision table for a gated method (default: POST):
//
//	no key                → pass through (or 400 with --require-key)
//	invalid key           → 400
//	stored, same request  → replay the stored response, backend untouched
//	stored, different req → 422 (key reuse with a new payload is a bug)
//	in flight, same req   → 409 + Retry-After (original still executing)
//	in flight, different  → 422
//	fresh                 → forward, then store the response
//
// Responses with status >= 500, upstream connection failures and bodies
// over the configured caps are never stored: the attempt stays retryable.
// Everything else — including 4xx — is stored, because a definitive
// answer must stay definitive across retries.
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/JaydenCJ/idemgate/internal/config"
	"github.com/JaydenCJ/idemgate/internal/policy"
	"github.com/JaydenCJ/idemgate/internal/store"
)

// ReplayedHeader marks responses served from the store instead of the
// backend, so clients and dashboards can tell a replay from an execution.
const ReplayedHeader = "Idempotency-Replayed"

// retryAfterSeconds is the hint sent with 409 while the original request
// is still executing.
const retryAfterSeconds = "1"

// Gate is the idempotency-deduplicating reverse proxy handler.
type Gate struct {
	Config *config.Config
	Store  *store.Store

	// Transport performs upstream round trips. The default never
	// consults proxy environment variables and never decompresses, so
	// stored bytes are exactly what the backend sent.
	Transport http.RoundTripper

	// Logger, when set, receives one line per gate decision. Keys are
	// logged as hash prefixes, never verbatim.
	Logger *log.Logger
}

// New returns a Gate with the default transport and no logger.
func New(cfg *config.Config, st *store.Store) *Gate {
	return &Gate{
		Config: cfg,
		Store:  st,
		Transport: &http.Transport{
			Proxy:               nil, // deliberate: ignore HTTP(S)_PROXY env
			DisableCompression:  true,
			MaxIdleConnsPerHost: 32,
		},
	}
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.Config.Methods.Gated(r.Method) {
		g.passThrough(w, r)
		return
	}
	raw := r.Header.Get(g.Config.HeaderName)
	if raw == "" {
		if g.Config.RequireKey {
			problem(w, http.StatusBadRequest, "idempotency key required",
				fmt.Sprintf("%s requests to this service must carry an %s header", r.Method, g.Config.HeaderName))
			return
		}
		g.passThrough(w, r)
		return
	}
	key, err := policy.ValidateKey(raw)
	if err != nil {
		problem(w, http.StatusBadRequest, "invalid idempotency key", err.Error())
		return
	}
	g.gated(w, r, key)
}

// gated handles one request that carries a valid key.
func (g *Gate) gated(w http.ResponseWriter, r *http.Request, key string) {
	body, tooLarge, err := readBody(r.Body, g.Config.MaxRequest)
	if err != nil {
		problem(w, http.StatusBadRequest, "unreadable request body", err.Error())
		return
	}
	if tooLarge {
		problem(w, http.StatusRequestEntityTooLarge, "request too large to deduplicate",
			fmt.Sprintf("keyed request bodies are capped at %d bytes (--max-request)", g.Config.MaxRequest))
		return
	}
	fingerprint := policy.Fingerprint(r.Method, r.URL.RequestURI(), body)

	if g.replayIfStored(w, r, key, fingerprint) {
		return
	}

	lease, err := g.Store.Acquire(key, fingerprint)
	if err != nil {
		var inflight *store.InFlightError
		if errors.As(err, &inflight) {
			if inflight.Fingerprint != fingerprint {
				g.mismatch(w, r, key)
				return
			}
			g.logf("conflict key=%s %s %s (original still in flight)", shortKey(key), r.Method, r.URL.Path)
			problemRetry(w, http.StatusConflict, "request already in flight",
				"a request with this idempotency key is still executing; retry shortly",
				retryAfterSeconds)
			return
		}
		problem(w, http.StatusInternalServerError, "idempotency store error", err.Error())
		return
	}

	// Double-check after winning the lease: a previous holder may have
	// committed between our Get and Acquire.
	if g.replayIfStored(w, r, key, fingerprint) {
		lease.Abandon()
		return
	}

	resp, err := g.forward(r, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		lease.Abandon()
		g.logf("upstream error key=%s %s %s: %v", shortKey(key), r.Method, r.URL.Path, err)
		problem(w, http.StatusBadGateway, "upstream unreachable", "the backend did not answer; nothing was stored, so this request is safe to retry")
		return
	}
	defer resp.Body.Close()
	stripHopByHop(resp.Header)

	if resp.StatusCode >= 500 {
		// Server errors stay retryable: forward, store nothing.
		g.stream(w, resp, false)
		lease.Abandon()
		g.logf("not stored key=%s status=%d %s %s (5xx is retryable)", shortKey(key), resp.StatusCode, r.Method, r.URL.Path)
		return
	}

	captured, complete := g.stream(w, resp, true)
	if !complete {
		lease.Abandon()
		g.logf("not stored key=%s status=%d %s %s (body exceeded --max-response or transfer aborted)",
			shortKey(key), resp.StatusCode, r.Method, r.URL.Path)
		return
	}
	if err := lease.Commit(resp.StatusCode, resp.Header, captured); err != nil {
		g.logf("store failed key=%s: %v", shortKey(key), err)
		return
	}
	g.logf("stored key=%s status=%d %s %s (%d bytes)", shortKey(key), resp.StatusCode, r.Method, r.URL.Path, len(captured))
}

// replayIfStored answers from the store when a record exists. It reports
// whether the request was fully handled (replay, mismatch, or store
// error).
func (g *Gate) replayIfStored(w http.ResponseWriter, r *http.Request, key, fingerprint string) bool {
	rec, err := g.Store.Get(key)
	if err != nil {
		problem(w, http.StatusInternalServerError, "idempotency store error", err.Error())
		return true
	}
	if rec == nil {
		return false
	}
	if rec.Fingerprint != fingerprint {
		g.mismatch(w, r, key)
		return true
	}
	g.replay(w, rec)
	g.logf("replayed key=%s status=%d %s %s", shortKey(key), rec.Status, r.Method, r.URL.Path)
	return true
}

// replay writes a stored response. Content-Length is recomputed from the
// stored body; everything else is served exactly as the backend sent it.
func (g *Gate) replay(w http.ResponseWriter, rec *store.Record) {
	h := w.Header()
	copyHeader(h, rec.Header)
	h.Del("Content-Length")
	h.Set("Content-Length", strconv.Itoa(len(rec.Body)))
	h.Set(ReplayedHeader, "true")
	w.WriteHeader(rec.Status)
	_, _ = w.Write(rec.Body)
}

func (g *Gate) mismatch(w http.ResponseWriter, r *http.Request, key string) {
	g.logf("fingerprint mismatch key=%s %s %s", shortKey(key), r.Method, r.URL.Path)
	problem(w, http.StatusUnprocessableEntity, "idempotency key reused",
		"this idempotency key was already used with a different method, path or body; use a fresh key for a new request")
}

// passThrough proxies without touching the store: ungated methods, and
// keyless requests when keys are optional. Bodies stream in both
// directions and are never buffered.
func (g *Gate) passThrough(w http.ResponseWriter, r *http.Request) {
	resp, err := g.forward(r, r.Body, r.ContentLength)
	if err != nil {
		problem(w, http.StatusBadGateway, "upstream unreachable", "the backend did not answer")
		return
	}
	defer resp.Body.Close()
	stripHopByHop(resp.Header)
	g.stream(w, resp, false)
}

// forward performs one upstream round trip with proxy hygiene applied:
// hop-by-hop headers stripped, X-Forwarded-* and Via set, original Host
// preserved.
func (g *Gate) forward(r *http.Request, body io.Reader, contentLength int64) (*http.Response, error) {
	target := *g.Config.Upstream
	target.Path = singleJoiningSlash(g.Config.Upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		return nil, err
	}
	out.ContentLength = contentLength
	out.Header = r.Header.Clone()
	stripHopByHop(out.Header)
	setForwarded(out.Header, r)
	out.Host = r.Host
	return g.Transport.RoundTrip(out)
}

// stream copies status, headers and body to the client. With capture on,
// it also accumulates the body up to MaxResponse; the returned bool is
// true only when the whole body fit and reached the client, i.e. only
// then is the copy safe to store.
func (g *Gate) stream(w http.ResponseWriter, resp *http.Response, capture bool) ([]byte, bool) {
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var buf bytes.Buffer
	complete := capture
	chunk := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			if complete {
				if int64(buf.Len()+n) > g.Config.MaxResponse {
					complete = false
					buf = bytes.Buffer{} // free the partial copy
				} else {
					buf.Write(chunk[:n])
				}
			}
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return nil, false // client went away; do not store a response it never got
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, false // backend died mid-body; the copy is incomplete
		}
	}
	if !capture {
		return nil, false
	}
	return buf.Bytes(), complete
}

// readBody buffers a request body up to max bytes. The bool result is
// true when the body was larger than max.
func readBody(rc io.ReadCloser, max int64) ([]byte, bool, error) {
	if rc == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return nil, true, nil
	}
	return data, false, nil
}

// shortKey is the loggable form of a key: the first 12 hex digits of its
// hash. Raw keys never appear in logs.
func shortKey(key string) string {
	return store.HashKey(key)[:12]
}

func (g *Gate) logf(format string, args ...any) {
	if g.Logger != nil {
		g.Logger.Printf("idemgate: "+format, args...)
	}
}

// compile-time interface check
var _ http.Handler = (*Gate)(nil)
