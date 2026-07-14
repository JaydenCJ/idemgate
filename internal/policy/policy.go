// Package policy decides which requests idemgate gates and how a request
// is reduced to a fingerprint.
//
// The rules are deliberately conservative and deliberately dumb: a key is
// a short piece of visible ASCII, only explicitly-listed unsafe methods
// are ever gated, and the fingerprint is an exact hash of method, request
// target and body with no canonicalization. Two requests that differ in
// any byte the client controls are different requests — guessing at
// equivalence (reordered query parameters, equivalent JSON) is how
// idempotency layers end up replaying the wrong response.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// MaxKeyLength is the longest accepted Idempotency-Key, in bytes.
// It matches the ceiling large payment APIs advertise, so keys that work
// there work here.
const MaxKeyLength = 255

// ValidateKey checks a raw Idempotency-Key header value and returns the
// canonical key. A key wrapped in double quotes (the IETF draft header
// syntax) is unwrapped, so `"abc"` and `abc` name the same record.
func ValidateKey(raw string) (string, error) {
	key := raw
	if len(key) >= 2 && strings.HasPrefix(key, `"`) && strings.HasSuffix(key, `"`) {
		key = key[1 : len(key)-1]
	}
	if key == "" {
		return "", fmt.Errorf("idempotency key is empty")
	}
	if len(key) > MaxKeyLength {
		return "", fmt.Errorf("idempotency key is %d bytes; the maximum is %d", len(key), MaxKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if b := key[i]; b <= 0x20 || b >= 0x7f {
			return "", fmt.Errorf("idempotency key contains byte 0x%02x at position %d; only visible ASCII is allowed", b, i)
		}
	}
	return key, nil
}

// Fingerprint reduces a request to a stable identity: the method, the
// exact request target (path plus raw query, no reordering or decoding)
// and a digest of the body. Reusing a key with a different fingerprint is
// a client bug and is answered with 422, so the fingerprint must never
// depend on anything volatile — header order, dates, connection details.
func Fingerprint(method, requestURI string, body []byte) string {
	bodySum := sha256.Sum256(body)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", method, requestURI, hex.EncodeToString(bodySum[:]))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// MethodSet is the set of HTTP methods the gate applies to. Keys are
// upper-case method names.
type MethodSet map[string]bool

// safeMethods must never be gated: replaying a stored response to a GET
// would turn the proxy into a broken cache, and safe methods carry no
// side effects worth deduplicating.
var safeMethods = map[string]bool{
	"GET":     true,
	"HEAD":    true,
	"OPTIONS": true,
	"TRACE":   true,
}

// ParseMethods parses a comma-separated method list such as "POST,PATCH".
// Names are case-insensitive; safe methods are rejected outright.
func ParseMethods(list string) (MethodSet, error) {
	set := make(MethodSet)
	for _, part := range strings.Split(list, ",") {
		m := strings.ToUpper(strings.TrimSpace(part))
		if m == "" {
			return nil, fmt.Errorf("empty method in list %q", list)
		}
		for i := 0; i < len(m); i++ {
			if m[i] < 'A' || m[i] > 'Z' {
				return nil, fmt.Errorf("invalid method %q: methods are ASCII letters only", part)
			}
		}
		if safeMethods[m] {
			return nil, fmt.Errorf("refusing to gate safe method %s: replaying it would break read semantics", m)
		}
		set[m] = true
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("method list %q is empty", list)
	}
	return set, nil
}

// Gated reports whether requests with this method go through the
// idempotency gate.
func (m MethodSet) Gated(method string) bool {
	return m[strings.ToUpper(method)]
}

// String renders the set in a stable, comma-separated order for banners
// and error messages.
func (m MethodSet) String() string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
