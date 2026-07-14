// Unit tests for the file-backed store: lookup, atomic writes, lazy
// expiry, lease arbitration and crash recovery. All time-dependent
// behavior runs against an injected clock — nothing here sleeps.
package store

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testStore returns an initialized store with a controllable clock.
// Advance the clock through the returned pointer.
func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	s := New(t.TempDir(), 24*time.Hour, 30*time.Second)
	s.Now = func() time.Time { return now }
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s, &now
}

func putSample(t *testing.T, s *Store, key string) *Record {
	t.Helper()
	now := s.Now()
	rec := &Record{
		Key:         key,
		Fingerprint: "sha256:aa",
		Status:      201,
		StoredAt:    now,
		ExpiresAt:   now.Add(s.TTL),
		Header:      http.Header{"Content-Type": {"application/json"}},
		Body:        []byte(`{"ok":true}`),
	}
	if err := s.Put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	return rec
}

func TestGetMissingReturnsNil(t *testing.T) {
	s, _ := testStore(t)
	rec, err := s.Get("nope")
	if err != nil || rec != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", rec, err)
	}
}

func TestPutThenGetRoundTrips(t *testing.T) {
	s, _ := testStore(t)
	putSample(t, s, "order-42")
	got, err := s.Get("order-42")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Status != 201 || string(got.Body) != `{"ok":true}` {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestGetDeletesExpiredRecord(t *testing.T) {
	s, now := testStore(t)
	putSample(t, s, "order-42")
	*now = now.Add(25 * time.Hour)

	rec, err := s.Get("order-42")
	if err != nil || rec != nil {
		t.Fatalf("expired record still served: (%v, %v)", rec, err)
	}
	if _, err := os.Stat(s.recordPath(HashKey("order-42"))); !os.IsNotExist(err) {
		t.Fatal("expired record file not lazily deleted")
	}
}

func TestGetHonorsExpiryBoundary(t *testing.T) {
	s, now := testStore(t)
	putSample(t, s, "order-42")
	*now = now.Add(24*time.Hour - time.Second)
	if rec, _ := s.Get("order-42"); rec == nil {
		t.Fatal("record expired one second early")
	}
	*now = now.Add(time.Second)
	if rec, _ := s.Get("order-42"); rec != nil {
		t.Fatal("record survived past its TTL")
	}
}

func TestGetFailsClosedOnCorruptRecord(t *testing.T) {
	// A corrupt record must be an error, not a miss: a miss would
	// silently re-execute a request that may already have happened.
	s, _ := testStore(t)
	putSample(t, s, "order-42")
	path := s.recordPath(HashKey("order-42"))
	if err := os.WriteFile(path, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("order-42"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt record not surfaced as an error: %v", err)
	}
}

func TestPutLeavesNoTempFiles(t *testing.T) {
	s, _ := testStore(t)
	putSample(t, s, "order-42")
	var stray []string
	filepath.Walk(s.Dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(filepath.Base(path), ".tmp-") {
			stray = append(stray, path)
		}
		return nil
	})
	if len(stray) > 0 {
		t.Fatalf("temp files left behind: %v", stray)
	}
}

func TestHostileKeysStayInsideStoreDir(t *testing.T) {
	// Keys are client-chosen; hashing must keep "../../etc/passwd"
	// from escaping the store directory.
	s, _ := testStore(t)
	key := "../../etc/passwd"
	putSample(t, s, key)
	path := s.recordPath(HashKey(key))
	abs, _ := filepath.Abs(path)
	root, _ := filepath.Abs(s.Dir)
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		t.Fatalf("record escaped the store dir: %s", abs)
	}
	if rec, err := s.Get(key); err != nil || rec == nil {
		t.Fatalf("hostile key not retrievable: (%v, %v)", rec, err)
	}
	if HashKey("a") == HashKey("b") || HashKey("a") != HashKey("a") {
		t.Fatal("key hashing is not stable and collision-free for distinct keys")
	}
}

func TestAcquireBlocksSecondHolder(t *testing.T) {
	s, _ := testStore(t)
	lease, err := s.Acquire("order-42", "sha256:aa")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lease.Abandon()

	_, err = s.Acquire("order-42", "sha256:aa")
	inflight, ok := err.(*InFlightError)
	if !ok {
		t.Fatalf("expected *InFlightError, got %v", err)
	}
	if inflight.Fingerprint != "sha256:aa" {
		t.Fatalf("holder fingerprint not reported: %q", inflight.Fingerprint)
	}
}

func TestAcquireReportsHolderFingerprint(t *testing.T) {
	// The caller uses the holder's fingerprint to pick 409 vs 422.
	s, _ := testStore(t)
	lease, _ := s.Acquire("order-42", "sha256:original")
	defer lease.Abandon()
	_, err := s.Acquire("order-42", "sha256:different")
	inflight, ok := err.(*InFlightError)
	if !ok || inflight.Fingerprint != "sha256:original" {
		t.Fatalf("expected in-flight error carrying the original fingerprint, got %v", err)
	}
}

func TestAbandonFreesTheKey(t *testing.T) {
	s, _ := testStore(t)
	lease, _ := s.Acquire("order-42", "sha256:aa")
	lease.Abandon()
	lease.Abandon() // idempotent

	again, err := s.Acquire("order-42", "sha256:aa")
	if err != nil {
		t.Fatalf("acquire after abandon: %v", err)
	}
	again.Abandon()
	if _, err := os.Stat(s.leasePath(HashKey("order-42"))); !os.IsNotExist(err) {
		t.Fatal("lease file not removed")
	}
}

func TestCommitStoresAndReleases(t *testing.T) {
	s, _ := testStore(t)
	lease, _ := s.Acquire("order-42", "sha256:aa")
	err := lease.Commit(201, http.Header{"Content-Type": {"application/json"}}, []byte(`{"id":1}`))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	rec, err := s.Get("order-42")
	if err != nil || rec == nil {
		t.Fatalf("committed record not readable: (%v, %v)", rec, err)
	}
	if !rec.ExpiresAt.Equal(rec.StoredAt.Add(24 * time.Hour)) {
		t.Fatalf("TTL not applied: %v -> %v", rec.StoredAt, rec.ExpiresAt)
	}
	if _, err := s.Acquire("order-42", "sha256:aa"); err != nil {
		t.Fatalf("key still locked after commit: %v", err)
	}
}

func TestStaleLeaseIsStolenAfterRestart(t *testing.T) {
	// Simulate a crash: a lease file exists but no process holds it.
	// A fresh Store (empty in-memory map) must refuse while the lease
	// is fresh and steal it once it is older than LeaseTimeout.
	s, now := testStore(t)
	if _, err := s.Acquire("order-42", "sha256:aa"); err != nil {
		t.Fatalf("seed acquire: %v", err)
	}

	restarted := New(s.Dir, s.TTL, s.LeaseTimeout)
	restarted.Now = func() time.Time { return *now }

	if _, err := restarted.Acquire("order-42", "sha256:aa"); err == nil {
		t.Fatal("fresh lease from the crashed process was stolen too early")
	}
	*now = now.Add(31 * time.Second)
	lease, err := restarted.Acquire("order-42", "sha256:aa")
	if err != nil {
		t.Fatalf("stale lease not stolen: %v", err)
	}
	lease.Abandon()
}

func TestCorruptLeaseFileIsStolen(t *testing.T) {
	s, _ := testStore(t)
	path := s.leasePath(HashKey("order-42"))
	if err := os.WriteFile(path, []byte("not a lease"), 0o644); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire("order-42", "sha256:aa")
	if err != nil {
		t.Fatalf("corrupt lease blocked acquisition: %v", err)
	}
	lease.Abandon()
}

func TestAcquireRecreatesDeletedLeasesDir(t *testing.T) {
	// An operator's rm -rf on the store must not wedge a running proxy:
	// Put recreates the records tree on demand, and Acquire gives the
	// leases directory the same self-healing treatment.
	s, _ := testStore(t)
	if err := os.RemoveAll(filepath.Join(s.Dir, "leases")); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire("order-42", "sha256:aa")
	if err != nil {
		t.Fatalf("acquire after leases dir was deleted: %v", err)
	}
	lease.Abandon()
}

func TestRemoveClearsRecordAndLease(t *testing.T) {
	s, _ := testStore(t)
	putSample(t, s, "order-42")
	if _, err := s.Acquire("order-42", "sha256:aa"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	found, err := s.Remove("order-42")
	if err != nil || !found {
		t.Fatalf("remove: (%v, %v)", found, err)
	}
	if rec, _ := s.Get("order-42"); rec != nil {
		t.Fatal("record survived rm")
	}
	// rm must unstick the key even though a lease existed.
	lease, err := s.Acquire("order-42", "sha256:bb")
	if err != nil {
		t.Fatalf("key still wedged after rm: %v", err)
	}
	lease.Abandon()
}

func TestRemoveMissingReportsFalse(t *testing.T) {
	s, _ := testStore(t)
	found, err := s.Remove("nope")
	if err != nil || found {
		t.Fatalf("expected (false, nil), got (%v, %v)", found, err)
	}
}

func TestListSortsByKeyAndReportsCorrupt(t *testing.T) {
	s, _ := testStore(t)
	putSample(t, s, "zeta")
	putSample(t, s, "alpha")
	badPath := s.recordPath(HashKey("broken"))
	os.MkdirAll(filepath.Dir(badPath), 0o755)
	os.WriteFile(badPath, []byte("garbage\n"), 0o644)

	records, corrupt, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 2 || records[0].Key != "alpha" || records[1].Key != "zeta" {
		t.Fatalf("unexpected listing: %+v", records)
	}
	if len(corrupt) != 1 || !strings.Contains(corrupt[0], HashKey("broken")[:2]) {
		t.Fatalf("corrupt file not reported: %v", corrupt)
	}
}

func TestPurgeSweepsExpiredAndStaleLeases(t *testing.T) {
	s, now := testStore(t)
	putSample(t, s, "old")
	*now = now.Add(12 * time.Hour)
	putSample(t, s, "fresh")
	// Leave a crashed lease behind by acquiring with one store instance
	// and purging with another.
	if _, err := s.Acquire("crashed", "sha256:aa"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	*now = now.Add(13 * time.Hour) // "old" expired, "fresh" still live, lease stale

	sweeper := New(s.Dir, s.TTL, s.LeaseTimeout)
	sweeper.Now = func() time.Time { return *now }
	stats, err := sweeper.Purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if stats.Removed != 1 || stats.Kept != 1 || stats.StaleLeases != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if rec, _ := s.Get("fresh"); rec == nil {
		t.Fatal("live record purged")
	}
}

func TestPurgeLeavesCorruptRecordsInPlace(t *testing.T) {
	// Destroying evidence of what may have been replayed is an operator
	// decision, not a sweep's.
	s, _ := testStore(t)
	badPath := s.recordPath(HashKey("broken"))
	os.MkdirAll(filepath.Dir(badPath), 0o755)
	os.WriteFile(badPath, []byte("garbage\n"), 0o644)

	stats, err := s.Purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if stats.Corrupt != 1 {
		t.Fatalf("corrupt record not counted: %+v", stats)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatal("corrupt record deleted by purge")
	}
}

func TestPurgeSkipsLiveInProcessLeases(t *testing.T) {
	s, now := testStore(t)
	lease, _ := s.Acquire("busy", "sha256:aa")
	defer lease.Abandon()
	*now = now.Add(time.Hour) // stale by timestamp, but held in-process

	stats, err := s.Purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if stats.StaleLeases != 0 {
		t.Fatalf("purge cleared a live lease: %+v", stats)
	}
}
