// Hop-by-hop stripping and forwarding-header hygiene.
//
// idemgate is a real proxy, so it follows RFC 9110 §7.6.1: connection-
// scoped headers must not be forwarded (or stored — a replayed
// Keep-Alive header would describe a connection that no longer exists),
// and the standard X-Forwarded-* trio tells the backend who really
// called.
package proxy

import (
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// hopByHop is the RFC 9110 connection-scoped header set, plus the legacy
// Proxy-Connection.
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop removes connection-scoped headers, including any named by
// the Connection header itself.
func stripHopByHop(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(textproto.CanonicalMIMEHeaderKey(token))
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

// copyHeader appends every value of src into dst, preserving per-name
// value order.
func copyHeader(dst, src http.Header) {
	for name, values := range src {
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// setForwarded stamps X-Forwarded-For/Host/Proto and a Via entry on the
// outbound headers. An existing X-Forwarded-For chain is extended, not
// replaced.
func setForwarded(h http.Header, r *http.Request) {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := h.Get("X-Forwarded-For"); prior != "" {
			host = prior + ", " + host
		}
		h.Set("X-Forwarded-For", host)
	}
	h.Set("X-Forwarded-Host", r.Host)
	h.Set("X-Forwarded-Proto", "http")
	h.Add("Via", "1.1 idemgate")
}

// singleJoiningSlash joins an upstream path prefix and a request path
// with exactly one slash between them.
func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash && b != "":
		return a + "/" + b
	}
	return a + b
}
