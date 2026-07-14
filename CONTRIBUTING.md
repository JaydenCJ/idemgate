# Contributing to idemgate

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go 1.22 or newer; there are no other dependencies of any kind
(the smoke test additionally uses `curl` and `bash`).

```bash
git clone https://github.com/JaydenCJ/idemgate.git
cd idemgate
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the proxy and the example backend, then drives
the full lifecycle over loopback — execute → replay → 422 → restart →
rm → re-execute; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (all 88 tests).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   packages (`policy`, `store`) rather than in the proxy or CLI layer.

## Ground rules

- Zero runtime dependencies is a core feature: the `go.mod` require list
  stays empty. Adding a dependency needs strong justification in the PR.
- The gate must fail safe. Anything ambiguous — corrupt records, torn
  writes, oversized bodies — must surface as an explicit error or a
  retryable non-stored pass, never as a silent duplicate execution and
  never as a silent replay of the wrong bytes.
- No network calls other than to the configured upstream; no telemetry.
  The proxy binds loopback by default and ignores proxy environment
  variables on purpose.
- Raw idempotency keys never appear in logs — log the hash prefix.
- The record format is versioned; any change to it bumps the format
  version and keeps `v1` decoding intact.
- Code comments and doc comments are written in English.

## Reporting bugs

Please include the output of `idemgate --version`, the serve flags you
used, the relevant stderr log lines (keys appear only as hashes), and a
minimal request sequence — ideally two `curl` commands — that reproduces
the issue.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
