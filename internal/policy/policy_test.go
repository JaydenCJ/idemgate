// Unit tests for key validation, method sets and request fingerprinting.
// The fingerprint tests pin the exact sensitivity contract: any byte the
// client controls changes the fingerprint, nothing else does.
package policy

import (
	"strings"
	"testing"
)

func TestValidateKeyAcceptsTypicalKey(t *testing.T) {
	key, err := ValidateKey("order-42_abc.DEF~x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "order-42_abc.DEF~x" {
		t.Fatalf("key mangled: %q", key)
	}
}

func TestValidateKeyUnwrapsQuotedForm(t *testing.T) {
	// The IETF draft header syntax quotes the key; both spellings must
	// name the same record.
	key, err := ValidateKey(`"order-42"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "order-42" {
		t.Fatalf("quotes not unwrapped: %q", key)
	}
}

func TestValidateKeyRejectsEmptyForms(t *testing.T) {
	// Both the bare empty string and an empty quoted key ("") are
	// nothing to deduplicate on.
	for _, raw := range []string{"", `""`} {
		if _, err := ValidateKey(raw); err == nil {
			t.Fatalf("empty form %q accepted", raw)
		}
	}
}

func TestValidateKeyLengthBoundary(t *testing.T) {
	// 255 bytes is the documented ceiling; 256 is over it.
	if _, err := ValidateKey(strings.Repeat("k", MaxKeyLength)); err != nil {
		t.Fatalf("255-byte key rejected: %v", err)
	}
	if _, err := ValidateKey(strings.Repeat("k", MaxKeyLength+1)); err == nil {
		t.Fatal("256-byte key accepted")
	}
}

func TestValidateKeyRejectsForbiddenBytes(t *testing.T) {
	// Spaces, control bytes and non-ASCII are all rejected: keys travel
	// in an HTTP header and name files (via their hash), so the accepted
	// alphabet is deliberately narrow.
	for _, raw := range []string{"order 42", "order\x0142", "order\t42", "órden-42"} {
		if _, err := ValidateKey(raw); err == nil {
			t.Fatalf("key %q accepted", raw)
		}
	}
}

func TestFingerprintIsStable(t *testing.T) {
	a := Fingerprint("POST", "/charges?x=1", []byte(`{"amount":1}`))
	b := Fingerprint("POST", "/charges?x=1", []byte(`{"amount":1}`))
	if a != b {
		t.Fatalf("same request produced different fingerprints: %s vs %s", a, b)
	}
}

func TestFingerprintFormat(t *testing.T) {
	fp := Fingerprint("POST", "/x", nil)
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+64 {
		t.Fatalf("unexpected fingerprint shape: %q", fp)
	}
}

func TestFingerprintSensitiveToBody(t *testing.T) {
	a := Fingerprint("POST", "/charges", []byte(`{"amount":1}`))
	b := Fingerprint("POST", "/charges", []byte(`{"amount":2}`))
	if a == b {
		t.Fatal("body change did not change the fingerprint")
	}
}

func TestFingerprintSensitiveToMethod(t *testing.T) {
	if Fingerprint("POST", "/x", nil) == Fingerprint("PATCH", "/x", nil) {
		t.Fatal("method change did not change the fingerprint")
	}
}

func TestFingerprintSensitiveToPath(t *testing.T) {
	if Fingerprint("POST", "/a", nil) == Fingerprint("POST", "/b", nil) {
		t.Fatal("path change did not change the fingerprint")
	}
}

func TestFingerprintSensitiveToQueryOrder(t *testing.T) {
	// Deliberate: no canonicalization. ?a=1&b=2 and ?b=2&a=1 are
	// different requests, because guessing equivalence risks replaying
	// the wrong response.
	if Fingerprint("POST", "/x?a=1&b=2", nil) == Fingerprint("POST", "/x?b=2&a=1", nil) {
		t.Fatal("query reordering did not change the fingerprint")
	}
}

func TestParseMethodsNormalizesCaseAndSpace(t *testing.T) {
	set, err := ParseMethods(" post , Patch ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !set.Gated("POST") || !set.Gated("patch") || set.Gated("PUT") {
		t.Fatalf("unexpected set: %v", set)
	}
}

func TestParseMethodsRejectsSafeMethod(t *testing.T) {
	for _, m := range []string{"GET", "head", "OPTIONS", "trace"} {
		if _, err := ParseMethods(m); err == nil {
			t.Fatalf("safe method %s accepted for gating", m)
		}
	}
}

func TestParseMethodsRejectsEmptyAndGarbage(t *testing.T) {
	for _, list := range []string{"", "POST,,PATCH", "PO ST", "P0ST"} {
		if _, err := ParseMethods(list); err == nil {
			t.Fatalf("method list %q accepted", list)
		}
	}
}

func TestMethodSetStringIsSorted(t *testing.T) {
	set, err := ParseMethods("PUT,DELETE,POST")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := set.String(); got != "DELETE,POST,PUT" {
		t.Fatalf("unstable String(): %q", got)
	}
}
