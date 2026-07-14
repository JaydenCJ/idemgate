# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-12

### Added

- `idemgate serve`: a reverse proxy that deduplicates requests carrying an
  `Idempotency-Key` header in front of any HTTP backend, with zero
  application-code changes.
- Stripe-shaped gate semantics: exact retries replay the stored response
  (marked `Idempotency-Replayed: true`); reusing a key with a different
  method, path or body is refused with 422; a retry that arrives while the
  original is still executing gets 409 with `Retry-After`; all
  gate-generated errors are RFC 9457 `application/problem+json`.
- File-backed record store: one atomically-written file per key
  (temp file + rename), sharded by SHA-256 so hostile keys can never
  escape the store directory; records survive restarts and crashes.
- Retention via `--ttl` (default 24h) with lazy expiry on read, plus
  crash-safe execution leases (`--lease-timeout`) so a wedged key always
  recovers.
- Fail-safe storage rules: 5xx responses, unreachable backends and bodies
  over `--max-request`/`--max-response` are never stored, so failed
  attempts stay retryable; corrupt records fail closed instead of
  re-executing.
- Real-proxy hygiene: RFC 9110 hop-by-hop headers stripped both ways,
  `X-Forwarded-For/Host/Proto` and `Via` set, streaming pass-through for
  ungated traffic, gated methods and header name configurable
  (`--methods`, `--header`, `--require-key`).
- Store administration: `idemgate ls`, `rm` (also unsticks leases) and
  `purge` (sweeps expired records and stale leases; leaves corrupt
  records in place for inspection).
- Privacy defaults: binds `127.0.0.1`, ignores proxy environment
  variables, no telemetry; logs key hashes, never raw keys.
- A deliberately non-idempotent example payments backend under
  `examples/backend` for demos and the smoke test.
- 88 deterministic offline tests (`go test ./...`) and an end-to-end
  `scripts/smoke.sh` that prints `SMOKE OK`.

[0.1.0]: https://github.com/JaydenCJ/idemgate/releases/tag/v0.1.0
