#!/usr/bin/env bash
# Narrated idemgate walkthrough against the example payments backend.
# Loopback only; builds into a temp dir and cleans up after itself.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
PIDS=()
cleanup() {
  local pid
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

CURL=(curl -sS --noproxy '*' --max-time 10)
BODY='{"amount":1999,"currency":"usd"}'

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

say "building idemgate and the example backend"
(cd "$ROOT" && go build -o "$WORKDIR/idemgate" ./cmd/idemgate)
(cd "$ROOT" && go build -o "$WORKDIR/backend" ./examples/backend)

say "starting the backend and the proxy on loopback"
"$WORKDIR/backend" --listen 127.0.0.1:0 >"$WORKDIR/backend.log" 2>&1 &
PIDS+=($!)
until grep -q 'backend listening' "$WORKDIR/backend.log" 2>/dev/null; do sleep 0.05; done
BACKEND_URL="$(sed -n 's/^backend listening on //p' "$WORKDIR/backend.log")"

"$WORKDIR/idemgate" serve --listen 127.0.0.1:0 --upstream "$BACKEND_URL" \
  --store "$WORKDIR/gate-store" >"$WORKDIR/proxy.log" 2>&1 &
PIDS+=($!)
until grep -q 'proxying' "$WORKDIR/proxy.log" 2>/dev/null; do sleep 0.05; done
PROXY_URL="$(sed -n 's/^idemgate .* proxying \(http:[^ ]*\) ->.*/\1/p' "$WORKDIR/proxy.log")"
head -1 "$WORKDIR/proxy.log"

say "1) first keyed POST — the backend executes it"
"${CURL[@]}" -H 'Idempotency-Key: order-42' -H 'Content-Type: application/json' \
  -d "$BODY" "$PROXY_URL/charges"; echo

say "2) exact retry — replayed from the store, backend untouched"
"${CURL[@]}" -i -H 'Idempotency-Key: order-42' -H 'Content-Type: application/json' \
  -d "$BODY" "$PROXY_URL/charges" | grep -Ei '^(HTTP|Idempotency-Replayed)|^\{'

say "3) same key, different amount — refused with 422"
"${CURL[@]}" -o /dev/null -w 'HTTP %{http_code}\n' \
  -H 'Idempotency-Key: order-42' -H 'Content-Type: application/json' \
  -d '{"amount":9999,"currency":"usd"}' "$PROXY_URL/charges"

say "4) two keyless POSTs — both execute (nothing to deduplicate on)"
"${CURL[@]}" -o /dev/null -H 'Content-Type: application/json' -d "$BODY" "$PROXY_URL/charges"
"${CURL[@]}" -o /dev/null -H 'Content-Type: application/json' -d "$BODY" "$PROXY_URL/charges"

say "backend truth: how many charges really happened?"
"${CURL[@]}" "$PROXY_URL/processed"

say "what the store knows"
"$WORKDIR/idemgate" ls --store "$WORKDIR/gate-store"

say "done — three executions, one replay, one refusal"
