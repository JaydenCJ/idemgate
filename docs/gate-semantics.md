# Gate semantics and the record format

This document pins down exactly what idemgate promises, in the order the
gate evaluates a request. The proxy code (`internal/proxy`) is a direct
transcription of these rules; the tests assert every row.

## 1. Which requests are gated

A request goes through the idempotency gate only when **both** hold:

1. its method is in `--methods` (default `POST`; safe methods GET, HEAD,
   OPTIONS and TRACE are rejected at configuration time — replaying a
   read would turn the proxy into a broken cache), and
2. it carries a non-empty `--header` header (default `Idempotency-Key`).

Everything else is proxied verbatim, streaming, with no buffering and no
store interaction. With `--require-key`, a gated-method request without a
key is refused with `400` instead of passed through.

## 2. Keys

- 1–255 bytes of visible ASCII (0x21–0x7E). No spaces, no control bytes,
  no UTF-8.
- A key wrapped in double quotes — the IETF
  `Idempotency-Key` draft header syntax — is unwrapped, so `"abc"` and
  `abc` name the same record.
- Keys are client secrets of a sort: on disk they name files via their
  SHA-256 (which also makes path traversal impossible), and in logs only
  the first 12 hex digits of that hash ever appear.

## 3. Fingerprints

Every gated request is reduced to
`sha256(method \n request-target \n sha256(body))`. There is **no**
canonicalization: reordered query parameters, equivalent JSON, or a
changed content type produce a different fingerprint. Guessing at request
equivalence is how idempotency layers replay the wrong response; idemgate
refuses to guess.

## 4. The decision table

| # | Store state for the key | Fingerprint | Answer |
| --- | --- | --- | --- |
| 1 | no record, no lease | — | forward; store the response (rules in §5) |
| 2 | record exists | matches | replay: stored status, headers and body, plus `Idempotency-Replayed: true` |
| 3 | record exists | differs | `422 Unprocessable Entity` |
| 4 | lease held (in flight) | matches holder's | `409 Conflict` + `Retry-After: 1` |
| 5 | lease held (in flight) | differs from holder's | `422 Unprocessable Entity` |

All gate-generated responses (400/409/413/422/500/502) are RFC 9457
`application/problem+json` documents with `Cache-Control: no-store`, so a
client can always distinguish a gate decision from a backend response.

## 5. What gets stored — and what never does

Stored and replayed: every completed response with status `< 500`,
including 4xx. A card decline is a definitive answer; retrying it must
not charge the card.

Never stored (the lease is released so a retry re-executes):

- responses with status `>= 500` — outages are retryable, not archival;
- upstream connection failures (the client sees `502`);
- request bodies over `--max-request` (the client sees `413` and the
  backend is never called);
- response bodies over `--max-response` — the client still receives every
  byte, streamed, but the record is discarded;
- transfers that abort mid-body in either direction — a partial response
  must never become the canonical answer.

A record that exists but cannot be decoded **fails closed**: the gate
answers `500` rather than silently re-executing a request that may
already have happened. `idemgate rm <key>` clears it after inspection.

## 6. Leases and crash recovery

Winning execution rights for a key means creating
`leases/<sha256(key)>.lease` with `O_EXCL` (plus an in-process map that
serializes same-process concurrency). The lease holds the fingerprint —
that is how rows 4 and 5 of the decision table are told apart — and a
timestamp. If the proxy crashes mid-request, the orphaned lease is stolen
by the next arrival once it is older than `--lease-timeout` (default
30s), so a crash never wedges a key. `rm` also clears leases, and `purge`
sweeps stale ones.

One store directory belongs to one proxy process in v0.1.0. The file
leases exist for crash recovery, not cross-process arbitration; running
replicas against a shared store is a roadmap item (flock-based leases).

## 7. Record format (`idemgate record v1`)

One file per key at `records/<aa>/<sha256(key)>.rec` (`aa` = first two
hex digits), written to a temp file and renamed, so readers never see a
torn write. The format is line-oriented text with an exact byte count for
the body, so it is greppable yet binary-safe:

```text
idemgate record v1
key: order-42
fingerprint: sha256:9f2c…
status: 201
stored: 2026-07-12T09:00:00Z
expires: 2026-07-13T09:00:00Z
headers: 2
Content-Type: application/json
Location: /charges/ch_1
body: 66
{"id":"ch_1","amount":1999,"currency":"usd","status":"succeeded"}
```

Rules:

- the key is percent-encoded on the `key:` line; header names are sorted
  (values keep their per-name order) so encoding is deterministic;
- `body: N` is followed by exactly N raw bytes and one final newline —
  a length mismatch, a truncated file or trailing garbage all refuse to
  decode, with an error naming the offending line;
- timestamps are second-precision UTC RFC 3339; a record expires exactly
  at `expires`, checked lazily on every read and swept by `purge`;
- hop-by-hop headers are stripped *before* storage, so a replay never
  advertises a connection that no longer exists. On replay,
  `Content-Length` is recomputed from the stored body.

## 8. Proxy behavior around the gate

- RFC 9110 hop-by-hop headers (`Connection` and friends, plus anything
  the `Connection` header names) are stripped from both requests and
  responses.
- `X-Forwarded-For` is appended to, `X-Forwarded-Host`/`X-Forwarded-Proto`
  are set, `Via: 1.1 idemgate` is added; the original `Host` is preserved.
- The upstream transport never decompresses (stored bytes are wire bytes)
  and never consults `HTTP_PROXY`/`HTTPS_PROXY` — traffic goes to
  `--upstream` and nowhere else.

## 9. Known limitations in 0.1.0

- HTTP trailers are not forwarded.
- One proxy process per store directory (see §6).
- The gate buffers gated request bodies (bounded by `--max-request`) to
  fingerprint them; ungated traffic streams unbuffered.
