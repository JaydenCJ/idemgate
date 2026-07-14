// Record encoding and decoding for the on-disk format (idemgate record v1).
//
// The format is a line-oriented text header followed by the raw response
// body, so records are greppable and reviewable while staying byte-exact
// for binary bodies. Decoding is strict: any deviation fails with a
// positioned error rather than replaying a half-parsed response, because
// replaying the wrong bytes is strictly worse than failing loudly.
package store

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const recordMagic = "idemgate record v1"

// maxHeaderLines caps the header count a decoder will accept, so a
// corrupt count cannot make it loop over garbage.
const maxHeaderLines = 4096

// Record is one stored idempotent response: the key that owns it, the
// fingerprint of the request that produced it, and the full response.
type Record struct {
	Key         string
	Fingerprint string
	Status      int
	StoredAt    time.Time
	ExpiresAt   time.Time
	Header      http.Header
	Body        []byte
}

// Expired reports whether the record is past its retention at the given
// instant. A record expires exactly at ExpiresAt, not one tick later.
func (r *Record) Expired(now time.Time) bool {
	return !r.ExpiresAt.After(now)
}

// Encode renders the record in format v1. Encoding is deterministic:
// header names are sorted, values keep their original per-name order, and
// timestamps are second-precision UTC.
func (r *Record) Encode() ([]byte, error) {
	if r.Key == "" {
		return nil, fmt.Errorf("record: key is empty")
	}
	if r.Status < 100 || r.Status > 599 {
		return nil, fmt.Errorf("record: status %d out of range", r.Status)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n", recordMagic)
	fmt.Fprintf(&b, "key: %s\n", url.QueryEscape(r.Key))
	fmt.Fprintf(&b, "fingerprint: %s\n", r.Fingerprint)
	fmt.Fprintf(&b, "status: %d\n", r.Status)
	fmt.Fprintf(&b, "stored: %s\n", r.StoredAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "expires: %s\n", r.ExpiresAt.UTC().Format(time.RFC3339))

	names := make([]string, 0, len(r.Header))
	count := 0
	for name, vals := range r.Header {
		names = append(names, name)
		count += len(vals)
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "headers: %d\n", count)
	for _, name := range names {
		for _, v := range r.Header[name] {
			if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(v, "\r\n") {
				return nil, fmt.Errorf("record: header %q has a value that cannot be stored", name)
			}
			fmt.Fprintf(&b, "%s: %s\n", name, v)
		}
	}
	fmt.Fprintf(&b, "body: %d\n", len(r.Body))
	b.Write(r.Body)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// decoder walks the raw bytes line by line, tracking the line number so
// every parse error points at the offending line.
type decoder struct {
	data []byte
	pos  int
	line int
}

func (d *decoder) errf(format string, args ...any) error {
	return fmt.Errorf("record: line %d: %s", d.line, fmt.Sprintf(format, args...))
}

// next returns the next \n-terminated line without its terminator.
func (d *decoder) next() (string, error) {
	if d.pos >= len(d.data) {
		d.line++
		return "", d.errf("unexpected end of record")
	}
	i := bytes.IndexByte(d.data[d.pos:], '\n')
	if i < 0 {
		d.line++
		return "", d.errf("line is not newline-terminated")
	}
	line := string(d.data[d.pos : d.pos+i])
	d.pos += i + 1
	d.line++
	if strings.Contains(line, "\r") {
		return "", d.errf("carriage return in record")
	}
	return line, nil
}

// field reads one "name: value" header-block line and enforces the name.
func (d *decoder) field(name string) (string, error) {
	line, err := d.next()
	if err != nil {
		return "", err
	}
	prefix := name + ": "
	if !strings.HasPrefix(line, prefix) {
		return "", d.errf("expected %q field, got %q", name, line)
	}
	return line[len(prefix):], nil
}

func (d *decoder) timeField(name string) (time.Time, error) {
	v, err := d.field(name)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, d.errf("invalid %s timestamp %q", name, v)
	}
	return t, nil
}

// Decode parses format v1 and returns the record, or a positioned error.
func Decode(data []byte) (*Record, error) {
	d := &decoder{data: data}

	magic, err := d.next()
	if err != nil {
		return nil, err
	}
	if magic != recordMagic {
		return nil, d.errf("not an idemgate v1 record (got %q)", magic)
	}

	rec := &Record{Header: make(http.Header)}

	escaped, err := d.field("key")
	if err != nil {
		return nil, err
	}
	if rec.Key, err = url.QueryUnescape(escaped); err != nil {
		return nil, d.errf("undecodable key %q", escaped)
	}
	if rec.Key == "" {
		return nil, d.errf("key is empty")
	}

	if rec.Fingerprint, err = d.field("fingerprint"); err != nil {
		return nil, err
	}
	if rec.Fingerprint == "" || strings.ContainsAny(rec.Fingerprint, " \t") {
		return nil, d.errf("malformed fingerprint %q", rec.Fingerprint)
	}

	statusText, err := d.field("status")
	if err != nil {
		return nil, err
	}
	if rec.Status, err = strconv.Atoi(statusText); err != nil || rec.Status < 100 || rec.Status > 599 {
		return nil, d.errf("invalid status %q", statusText)
	}

	if rec.StoredAt, err = d.timeField("stored"); err != nil {
		return nil, err
	}
	if rec.ExpiresAt, err = d.timeField("expires"); err != nil {
		return nil, err
	}

	countText, err := d.field("headers")
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count < 0 || count > maxHeaderLines {
		return nil, d.errf("invalid header count %q", countText)
	}
	for i := 0; i < count; i++ {
		line, err := d.next()
		if err != nil {
			return nil, err
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok || name == "" {
			return nil, d.errf("malformed header line %q", line)
		}
		rec.Header.Add(name, value)
	}

	lengthText, err := d.field("body")
	if err != nil {
		return nil, err
	}
	length, err := strconv.Atoi(lengthText)
	if err != nil || length < 0 {
		return nil, d.errf("invalid body length %q", lengthText)
	}
	rest := d.data[d.pos:]
	if len(rest) != length+1 || rest[length] != '\n' {
		return nil, d.errf("body is %d bytes on disk, header says %d", len(rest)-1, length)
	}
	rec.Body = append([]byte(nil), rest[:length]...)
	return rec, nil
}
