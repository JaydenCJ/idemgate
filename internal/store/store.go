// Package store persists idempotency records on the filesystem and
// arbitrates in-flight execution leases.
//
// Layout under the store directory:
//
//	records/<aa>/<sha256(key)>.rec   one file per key, format v1
//	leases/<sha256(key)>.lease       exists while a request is executing
//
// Keys are addressed by their SHA-256 so arbitrary client-chosen keys can
// never traverse outside the store directory. Writes are atomic
// (temp file + rename), reads apply expiry lazily, and the clock is an
// injectable function so every time-dependent behavior is testable
// without sleeping.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is a file-backed idempotency record store. A single Store owns
// its directory; running several idemgate processes against one directory
// is not supported in v0.1.0 (the on-disk leases only cover crash
// recovery, not live cross-process arbitration).
type Store struct {
	Dir          string
	TTL          time.Duration
	LeaseTimeout time.Duration

	// Now is the clock. Tests replace it; production uses time.Now.
	Now func() time.Time

	mu       sync.Mutex
	inflight map[string]string // key hash -> fingerprint
}

// New returns a Store rooted at dir. Call Init before serving traffic.
func New(dir string, ttl, leaseTimeout time.Duration) *Store {
	return &Store{
		Dir:          dir,
		TTL:          ttl,
		LeaseTimeout: leaseTimeout,
		Now:          time.Now,
		inflight:     make(map[string]string),
	}
}

// Init creates the store's directory skeleton.
func (s *Store) Init() error {
	for _, sub := range []string{"records", "leases"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, sub), 0o755); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// HashKey returns the hex SHA-256 of a key. It names files on disk and is
// what idemgate logs instead of the raw key, which may be sensitive.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *Store) recordPath(hash string) string {
	return filepath.Join(s.Dir, "records", hash[:2], hash+".rec")
}

func (s *Store) leasePath(hash string) string {
	return filepath.Join(s.Dir, "leases", hash+".lease")
}

// Get returns the live record for a key, or (nil, nil) when there is
// none. Expired records are deleted on sight. A record that exists but
// cannot be decoded is an error, not a miss: silently re-executing a
// request we may already have performed is the one failure mode an
// idempotency layer must never pick by default.
func (s *Store) Get(key string) (*Record, error) {
	path := s.recordPath(HashKey(key))
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	rec, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("store: corrupt record %s: %w (clear it with 'idemgate rm')", path, err)
	}
	if rec.Expired(s.Now()) {
		_ = os.Remove(path) // lazy expiry; Purge sweeps the rest
		return nil, nil
	}
	return rec, nil
}

// Put writes a record atomically: encode to a temp file in the target
// shard, then rename. Readers see either the old record or the new one,
// never a torn write.
func (s *Store) Put(rec *Record) error {
	data, err := rec.Encode()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	path := s.recordPath(HashKey(rec.Key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Remove deletes the record (and any lease) for a key. It reports whether
// a record existed. Clearing the lease too means rm can always unstick a
// key, even one wedged by a corrupt record or a crashed request.
func (s *Store) Remove(key string) (bool, error) {
	hash := HashKey(key)
	s.mu.Lock()
	delete(s.inflight, hash)
	s.mu.Unlock()
	_ = os.Remove(s.leasePath(hash))
	err := os.Remove(s.recordPath(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return true, nil
}

// InFlightError reports that another request holds the lease for a key.
// Fingerprint is what that request is executing, so callers can tell a
// plain concurrent retry (409) from a conflicting payload (422).
type InFlightError struct {
	Fingerprint string
}

func (e *InFlightError) Error() string {
	return "another request with this idempotency key is in flight"
}

// Lease is the exclusive right to execute a request for one key. Exactly
// one of Commit or Abandon must be called; both are idempotent.
type Lease struct {
	store       *Store
	hash        string
	key         string
	fingerprint string
	done        bool
}

// Acquire takes the execution lease for a key. It fails with
// *InFlightError while another holder is live. A lease file left behind
// by a crashed process is stolen once it is older than LeaseTimeout, so a
// crash never wedges a key forever.
func (s *Store) Acquire(key, fingerprint string) (*Lease, error) {
	hash := HashKey(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if current, ok := s.inflight[hash]; ok {
		return nil, &InFlightError{Fingerprint: current}
	}

	path := s.leasePath(hash)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := fmt.Fprintf(f, "%s\n%s\n", fingerprint, s.Now().UTC().Format(time.RFC3339))
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(path)
				return nil, fmt.Errorf("store: writing lease: %w", errors.Join(werr, cerr))
			}
			s.inflight[hash] = fingerprint
			return &Lease{store: s, hash: hash, key: key, fingerprint: fingerprint}, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			// The leases directory vanished under a running proxy (an
			// operator's rm -rf, an overeager tmp cleaner). Recreate it
			// and retry — Put self-heals the records tree the same way.
			if merr := os.MkdirAll(filepath.Dir(path), 0o755); merr != nil {
				return nil, fmt.Errorf("store: %w", merr)
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("store: %w", err)
		}
		heldFP, stamp, rerr := readLease(path)
		if rerr != nil || s.Now().Sub(stamp) > s.LeaseTimeout {
			// Unreadable or stale: the holder crashed. Steal and retry.
			os.Remove(path)
			continue
		}
		return nil, &InFlightError{Fingerprint: heldFP}
	}
	return nil, fmt.Errorf("store: lease for key hash %s is contended", hash[:12])
}

func readLease(path string) (fingerprint string, stamp time.Time, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		return "", time.Time{}, fmt.Errorf("store: truncated lease %s", path)
	}
	stamp, err = time.Parse(time.RFC3339, lines[1])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: bad lease timestamp in %s", path)
	}
	return lines[0], stamp, nil
}

// Commit stores the response under the lease's key and releases the
// lease. StoredAt/ExpiresAt come from the store clock and TTL.
func (l *Lease) Commit(status int, header http.Header, body []byte) error {
	now := l.store.Now()
	rec := &Record{
		Key:         l.key,
		Fingerprint: l.fingerprint,
		Status:      status,
		StoredAt:    now,
		ExpiresAt:   now.Add(l.store.TTL),
		Header:      header.Clone(),
		Body:        append([]byte(nil), body...),
	}
	if rec.Header == nil {
		rec.Header = make(http.Header)
	}
	err := l.store.Put(rec)
	l.release()
	return err
}

// Abandon releases the lease without storing anything, so a retry can
// re-execute (used for 5xx responses, upstream failures and oversize
// bodies).
func (l *Lease) Abandon() {
	l.release()
}

func (l *Lease) release() {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	delete(l.store.inflight, l.hash)
	os.Remove(l.store.leasePath(l.hash))
}

// List returns every decodable record (live and expired), sorted by key,
// plus the paths of any files that failed to decode. Listing never
// deletes anything.
func (s *Store) List() (records []*Record, corrupt []string, err error) {
	root := filepath.Join(s.Dir, "records")
	shards, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: %w", err)
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			return nil, nil, fmt.Errorf("store: %w", err)
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".rec") {
				continue
			}
			path := filepath.Join(root, shard.Name(), f.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, nil, fmt.Errorf("store: %w", err)
			}
			rec, derr := Decode(data)
			if derr != nil {
				corrupt = append(corrupt, path)
				continue
			}
			records = append(records, rec)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records, corrupt, nil
}

// PurgeStats summarizes one Purge sweep.
type PurgeStats struct {
	Removed     int // expired records deleted
	Kept        int // live records left in place
	Corrupt     int // undecodable records left in place (fail safe)
	StaleLeases int // crashed-holder lease files cleared
}

// Purge deletes expired records and stale lease files. Corrupt records
// are counted but deliberately not deleted — destroying evidence of what
// was replayed is an operator decision ('idemgate rm'), not a sweep's.
func (s *Store) Purge() (PurgeStats, error) {
	var stats PurgeStats
	now := s.Now()

	records, corrupt, err := s.List()
	if err != nil {
		return stats, err
	}
	stats.Corrupt = len(corrupt)
	for _, rec := range records {
		if rec.Expired(now) {
			if err := os.Remove(s.recordPath(HashKey(rec.Key))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return stats, fmt.Errorf("store: %w", err)
			}
			stats.Removed++
		} else {
			stats.Kept++
		}
	}

	leaseDir := filepath.Join(s.Dir, "leases")
	leases, err := os.ReadDir(leaseDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return stats, fmt.Errorf("store: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range leases {
		hash := strings.TrimSuffix(f.Name(), ".lease")
		if hash == f.Name() {
			continue
		}
		if _, live := s.inflight[hash]; live {
			continue // held by a request in this process
		}
		path := filepath.Join(leaseDir, f.Name())
		_, stamp, rerr := readLease(path)
		if rerr != nil || now.Sub(stamp) > s.LeaseTimeout {
			if err := os.Remove(path); err == nil {
				stats.StaleLeases++
			}
		}
	}
	return stats, nil
}
