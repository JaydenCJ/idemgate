// Unit tests for serve-flag parsing and validation. Configuration errors
// must be caught at startup with actionable messages — a proxy that boots
// with a half-valid config guards someone's payments incorrectly.
package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseServeAppliesDefaults(t *testing.T) {
	cfg, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Fatalf("default listen wrong: %q", cfg.Listen)
	}
	if cfg.StoreDir != ".idemgate" || cfg.TTL != 24*time.Hour || cfg.LeaseTimeout != 30*time.Second {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if !cfg.Methods.Gated("POST") || cfg.Methods.Gated("PATCH") {
		t.Fatalf("default methods wrong: %v", cfg.Methods)
	}
	if cfg.HeaderName != "Idempotency-Key" || cfg.RequireKey {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.MaxRequest != 1<<20 || cfg.MaxResponse != 8<<20 {
		t.Fatalf("default caps wrong: %d %d", cfg.MaxRequest, cfg.MaxResponse)
	}
}

func TestParseServeRequiresUpstream(t *testing.T) {
	_, err := ParseServe(nil)
	if err == nil || !strings.Contains(err.Error(), "--upstream is required") {
		t.Fatalf("missing upstream not caught: %v", err)
	}
}

func TestParseServeRejectsBadUpstreams(t *testing.T) {
	for _, bad := range []string{
		"ftp://127.0.0.1:9000",          // wrong scheme
		"http://",                       // no host
		"http://127.0.0.1:9000?a=1",     // query
		"http://127.0.0.1:9000#frag",    // fragment
		"http://user:pw@127.0.0.1:9000", // embedded credentials
		"127.0.0.1:9000",                // scheme-less
	} {
		if _, err := ParseServe([]string{"--upstream", bad}); err == nil {
			t.Fatalf("upstream %q accepted", bad)
		}
	}
}

func TestParseServeAcceptsPathPrefixUpstream(t *testing.T) {
	cfg, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000/api/v2"})
	if err != nil {
		t.Fatalf("path-prefix upstream rejected: %v", err)
	}
	if cfg.Upstream.Path != "/api/v2" {
		t.Fatalf("path prefix lost: %q", cfg.Upstream.Path)
	}
}

func TestParseServeParsesMethodList(t *testing.T) {
	cfg, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--methods", "post,PATCH,delete"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, m := range []string{"POST", "PATCH", "DELETE"} {
		if !cfg.Methods.Gated(m) {
			t.Fatalf("%s not gated: %v", m, cfg.Methods)
		}
	}
}

func TestParseServeRejectsGatingGet(t *testing.T) {
	_, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--methods", "GET"})
	if err == nil {
		t.Fatal("gating GET accepted")
	}
}

func TestParseServeRejectsNonPositiveDurations(t *testing.T) {
	if _, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--ttl", "0s"}); err == nil {
		t.Fatal("--ttl 0s accepted")
	}
	if _, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--lease-timeout", "-1s"}); err == nil {
		t.Fatal("negative --lease-timeout accepted")
	}
}

func TestParseServeRejectsBadHeaderName(t *testing.T) {
	for _, bad := range []string{"Bad Header", "", "Åccent-Key"} {
		if _, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--header", bad}); err == nil {
			t.Fatalf("header name %q accepted", bad)
		}
	}
}

func TestParseServeRejectsUnknownFlagAndStrayArgs(t *testing.T) {
	if _, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--bogus"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "stray"}); err == nil {
		t.Fatal("stray positional argument accepted")
	}
}

func TestParseSizeUnits(t *testing.T) {
	cases := map[string]int64{
		"1024":  1024,
		"64KiB": 64 << 10,
		"1MiB":  1 << 20,
		"2GiB":  2 << 30,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Fatalf("ParseSize(%q) = (%d, %v), want %d", in, got, err, want)
		}
	}
}

func TestParseSizeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "1MB", "MiB", "-5", "0", "1.5MiB", "9999999999GiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Fatalf("ParseSize(%q) accepted", bad)
		}
	}
}

func TestParseServeAppliesSizeFlags(t *testing.T) {
	cfg, err := ParseServe([]string{"--upstream", "http://127.0.0.1:9000", "--max-request", "16KiB", "--max-response", "2MiB"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxRequest != 16<<10 || cfg.MaxResponse != 2<<20 {
		t.Fatalf("size flags not applied: %d %d", cfg.MaxRequest, cfg.MaxResponse)
	}
}
