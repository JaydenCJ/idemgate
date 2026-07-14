// Gate-generated error responses, in RFC 9457 problem+json shape.
//
// Every response idemgate synthesizes itself (as opposed to forwarding or
// replaying) uses the same machine-readable format, so clients can tell a
// gate decision from a backend response by Content-Type alone.
package proxy

import (
	"encoding/json"
	"net/http"
)

// problemBody is an RFC 9457 problem details document. Field order is the
// declaration order, so encoded bodies are stable.
type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// problem writes a problem+json response.
func problem(w http.ResponseWriter, status int, title, detail string) {
	problemRetry(w, status, title, detail, "")
}

// problemRetry writes a problem+json response with an optional
// Retry-After header (used by the 409 in-flight answer).
func problemRetry(w http.ResponseWriter, status int, title, detail, retryAfter string) {
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(problemBody{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
	})
}
