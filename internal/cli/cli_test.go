// Integration tests for the CLI, run in-process through cli.Run with
// injected streams. Serve itself is exercised by scripts/smoke.sh; here
// we cover dispatch, flag validation and the store administration
// commands against real record directories.
package cli

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/idemgate/internal/store"
	"github.com/JaydenCJ/idemgate/internal/version"
)

// run invokes the CLI in-process and returns (exit code, stdout, stderr).
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// seedRecord writes a record with a fixed clock so listings are
// deterministic. storedAt in the past plus a short TTL yields a record
// that is deterministically expired at any real wall-clock time.
func seedRecord(t *testing.T, dir, key string, storedAt time.Time, ttl time.Duration) {
	t.Helper()
	st := store.New(dir, ttl, 30*time.Second)
	st.Now = func() time.Time { return storedAt }
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	err := st.Put(&store.Record{
		Key:         key,
		Fingerprint: "sha256:aa",
		Status:      201,
		StoredAt:    storedAt,
		ExpiresAt:   storedAt.Add(ttl),
		Header:      http.Header{"Content-Type": {"application/json"}},
		Body:        []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
}

var fixedStamp = time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)

func TestVersionSpellings(t *testing.T) {
	for _, arg := range []string{"--version", "-V", "version"} {
		code, out, _ := run(t, arg)
		if code != 0 || out != "idemgate "+version.Version+"\n" {
			t.Fatalf("%s: (%d, %q)", arg, code, out)
		}
	}
}

func TestHelpPrintsUsage(t *testing.T) {
	code, out, _ := run(t, "--help")
	if code != 0 || !strings.Contains(out, "Usage:") || !strings.Contains(out, "serve") {
		t.Fatalf("unexpected help output: (%d, %q)", code, out)
	}
}

func TestBadInvocationsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{},                  // no command
		{"frobnicate"},      // unknown command
		{"--bogus"},         // unknown global flag
		{"ls", "--bogus"},   // unknown subcommand flag
		{"ls", "stray-arg"}, // ls takes no positionals
		{"rm"},              // rm needs a key
	} {
		code, _, errb := run(t, args...)
		if code != exitError {
			t.Fatalf("args %v: exit %d, want %d (stderr %q)", args, code, exitError, errb)
		}
	}
}

func TestServeConfigErrorsExitTwo(t *testing.T) {
	code, _, errb := run(t, "serve")
	if code != exitError || !strings.Contains(errb, "--upstream is required") {
		t.Fatalf("missing upstream: (%d, %q)", code, errb)
	}
	code, _, errb = run(t, "serve", "--upstream", "http://127.0.0.1:9", "--methods", "GET")
	if code != exitError || !strings.Contains(errb, "safe method") {
		t.Fatalf("gating GET: (%d, %q)", code, errb)
	}
}

func TestLsListsRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gate-store")

	code, out, _ := run(t, "ls", "--store", dir)
	if code != 0 || !strings.Contains(out, "0 record(s)") {
		t.Fatalf("empty ls: (%d, %q)", code, out)
	}

	seedRecord(t, dir, "order-42", fixedStamp, 500000*time.Hour) // live far beyond any test run
	code, out, _ = run(t, "ls", "--store", dir)
	if code != 0 {
		t.Fatalf("ls failed: %d", code)
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "order-42") {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, "201") || !strings.Contains(line, "2026-07-12T09:00:00Z") || !strings.Contains(line, "live") {
		t.Fatalf("record row wrong: %q (full output %q)", line, out)
	}
	if !strings.Contains(out, "1 record(s)") {
		t.Fatalf("count line wrong: %q", out)
	}
}

func TestRmRemovesAndReportsMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gate-store")
	seedRecord(t, dir, "order-42", fixedStamp, time.Hour)

	code, out, _ := run(t, "rm", "--store", dir, "order-42")
	if code != 0 || !strings.Contains(out, "removed order-42") {
		t.Fatalf("rm existing: (%d, %q)", code, out)
	}
	code, _, errb := run(t, "rm", "--store", dir, "order-42")
	if code != exitFailure || !strings.Contains(errb, "no record") {
		t.Fatalf("rm missing: (%d, %q)", code, errb)
	}
}

func TestPurgeReportsCounts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gate-store")
	// Stored in 2026 with a 1h TTL: expired at any later wall-clock time.
	seedRecord(t, dir, "stale-key", fixedStamp, time.Hour)
	// A TTL of ~57 years keeps this one deterministically live.
	seedRecord(t, dir, "live-key", fixedStamp, 500000*time.Hour)

	code, out, _ := run(t, "purge", "--store", dir)
	if code != 0 || !strings.Contains(out, "purged 1 expired record(s); kept 1 live") {
		t.Fatalf("purge: (%d, %q)", code, out)
	}
	code, out, _ = run(t, "ls", "--store", dir)
	if code != 0 || strings.Contains(out, "stale-key") || !strings.Contains(out, "live-key") {
		t.Fatalf("post-purge listing wrong: %q", out)
	}
}
