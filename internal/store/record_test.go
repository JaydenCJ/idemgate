// Unit tests for the on-disk record format (idemgate record v1).
// Round-trips must be exact — including binary bodies — and every
// malformed input must fail with a positioned error rather than decode
// into something replayable.
package store

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sampleRecord() *Record {
	return &Record{
		Key:         "order-42",
		Fingerprint: "sha256:deadbeef",
		Status:      201,
		StoredAt:    time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
		ExpiresAt:   time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC),
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Location":     {"/charges/ch_1"},
		},
		Body: []byte(`{"id":"ch_1"}`),
	}
}

// encodeOK is a helper that fails the test on encoding errors.
func encodeOK(t *testing.T, rec *Record) []byte {
	t.Helper()
	data, err := rec.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

func TestRecordRoundTrip(t *testing.T) {
	rec := sampleRecord()
	got, err := Decode(encodeOK(t, rec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != rec.Key || got.Fingerprint != rec.Fingerprint || got.Status != rec.Status {
		t.Fatalf("identity fields mangled: %+v", got)
	}
	if !got.StoredAt.Equal(rec.StoredAt) || !got.ExpiresAt.Equal(rec.ExpiresAt) {
		t.Fatalf("timestamps mangled: %v %v", got.StoredAt, got.ExpiresAt)
	}
	if got.Header.Get("Location") != "/charges/ch_1" {
		t.Fatalf("headers mangled: %v", got.Header)
	}
	if !bytes.Equal(got.Body, rec.Body) {
		t.Fatalf("body mangled: %q", got.Body)
	}
}

func TestRecordEncodeIsDeterministic(t *testing.T) {
	rec := sampleRecord()
	if !bytes.Equal(encodeOK(t, rec), encodeOK(t, rec)) {
		t.Fatal("two encodes of the same record differ")
	}
}

func TestRecordBinaryBodyRoundTrips(t *testing.T) {
	// Bodies may be gzip or protobuf: embedded newlines and NULs must
	// survive byte-for-byte.
	rec := sampleRecord()
	rec.Body = []byte("\x00\x01\ngzip\r\n\xff\x00tail\n\n")
	got, err := Decode(encodeOK(t, rec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got.Body, rec.Body) {
		t.Fatalf("binary body mangled: %q", got.Body)
	}
}

func TestRecordEmptyBodyRoundTrips(t *testing.T) {
	rec := sampleRecord()
	rec.Body = nil
	got, err := Decode(encodeOK(t, rec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Body) != 0 {
		t.Fatalf("expected empty body, got %q", got.Body)
	}
}

func TestRecordMultiValueHeadersKeepOrder(t *testing.T) {
	rec := sampleRecord()
	rec.Header = http.Header{"Set-Cookie": {"a=1", "b=2", "c=3"}}
	got, err := Decode(encodeOK(t, rec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"a=1", "b=2", "c=3"}
	if len(got.Header["Set-Cookie"]) != 3 {
		t.Fatalf("values lost: %v", got.Header)
	}
	for i, v := range want {
		if got.Header["Set-Cookie"][i] != v {
			t.Fatalf("value order changed: %v", got.Header["Set-Cookie"])
		}
	}
}

func TestRecordHeaderValueWithColonRoundTrips(t *testing.T) {
	rec := sampleRecord()
	rec.Header = http.Header{"Link": {"<http://example.test/next>; rel=\"next\""}}
	got, err := Decode(encodeOK(t, rec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Header.Get("Link") != rec.Header.Get("Link") {
		t.Fatalf("colon value mangled: %q", got.Header.Get("Link"))
	}
}

func TestRecordKeyWithHostileCharsRoundTrips(t *testing.T) {
	// Keys are percent-encoded in the header block, so even characters
	// that would break a line-oriented format survive.
	rec := sampleRecord()
	rec.Key = "a/b\\c%20d+e&f=g#h"
	got, err := Decode(encodeOK(t, rec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != rec.Key {
		t.Fatalf("key mangled: %q", got.Key)
	}
}

func TestRecordEncodeRejectsNewlineInHeaderValue(t *testing.T) {
	rec := sampleRecord()
	rec.Header = http.Header{"X-Evil": {"a\nb"}}
	if _, err := rec.Encode(); err == nil {
		t.Fatal("newline in header value encoded without error")
	}
}

func TestRecordEncodeRejectsBadStatus(t *testing.T) {
	rec := sampleRecord()
	rec.Status = 999
	if _, err := rec.Encode(); err == nil {
		t.Fatal("status 999 encoded without error")
	}
}

func TestDecodeRejectsCorruptHeaderFields(t *testing.T) {
	// Each corruption must fail with an error that names the offending
	// line — "record is corrupt somewhere" is useless at 3 a.m.
	cases := []struct {
		name, old, new, wantLine string
	}{
		{"wrong magic", "idemgate record v1", "idemgate record v9", "line 1"},
		{"renamed field", "fingerprint: ", "fingerprnt: ", "line 3"},
		{"bad status", "status: 201", "status: banana", "line 4"},
		{"bad timestamp", "stored: 2026-07-12T09:00:00Z", "stored: yesterday-ish", "line 5"},
	}
	for _, tc := range cases {
		data := encodeOK(t, sampleRecord())
		data = bytes.Replace(data, []byte(tc.old), []byte(tc.new), 1)
		_, err := Decode(data)
		if err == nil || !strings.Contains(err.Error(), tc.wantLine) {
			t.Fatalf("%s: expected positioned error mentioning %q, got %v", tc.name, tc.wantLine, err)
		}
	}
}

func TestDecodeRejectsBodyLengthMismatch(t *testing.T) {
	// A truncated body must never decode: replaying half a response is
	// the worst possible outcome for an idempotency layer.
	data := encodeOK(t, sampleRecord())
	if _, err := Decode(data[:len(data)-3]); err == nil {
		t.Fatal("truncated body accepted")
	}
	if _, err := Decode(append(data, []byte("extra")...)); err == nil {
		t.Fatal("trailing garbage accepted")
	}
}

func TestDecodeRejectsMalformedHeaderLine(t *testing.T) {
	data := encodeOK(t, sampleRecord())
	data = bytes.Replace(data, []byte("Content-Type: application/json"), []byte("Content-Type=application/json"), 1)
	if _, err := Decode(data); err == nil {
		t.Fatal("malformed header line accepted")
	}
}

func TestDecodeRejectsInsaneHeaderCount(t *testing.T) {
	data := encodeOK(t, sampleRecord())
	data = bytes.Replace(data, []byte("headers: 2"), []byte("headers: 99999999"), 1)
	if _, err := Decode(data); err == nil {
		t.Fatal("absurd header count accepted")
	}
}

func TestDecodeRejectsTruncatedFile(t *testing.T) {
	if _, err := Decode([]byte("idemgate record v1\nkey: a\n")); err == nil {
		t.Fatal("truncated record accepted")
	}
}

func TestExpiredBoundary(t *testing.T) {
	// A record expires exactly at ExpiresAt: at that instant it is
	// already gone, one nanosecond earlier it is still live.
	rec := sampleRecord()
	at := rec.ExpiresAt
	if rec.Expired(at.Add(-time.Nanosecond)) {
		t.Fatal("record expired before ExpiresAt")
	}
	if !rec.Expired(at) {
		t.Fatal("record still live at ExpiresAt")
	}
}
