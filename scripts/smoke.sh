#!/usr/bin/env bash
# End-to-end smoke test for idemgate. No external network (loopback only),
# idempotent, runs from a clean tree. This script plus 'go test ./...' is
# the whole verification story — the repository intentionally ships no CI.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
PIDS=()

cleanup() {
  local pid
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

command -v curl >/dev/null || fail "curl is required to drive the proxy"
# Loopback traffic must never detour through a corporate proxy.
CURL=(curl -sS --noproxy '*' --max-time 10)

STORE="$WORKDIR/store"

wait_for_line() { # file pattern
  local i
  for i in $(seq 1 200); do
    grep -q "$2" "$1" 2>/dev/null && return 0
    sleep 0.05
  done
  fail "timed out waiting for '$2' in $1"
}

start_proxy() {
  : > "$WORKDIR/proxy.log"
  "$WORKDIR/idemgate" serve --listen 127.0.0.1:0 --upstream "$BACKEND_URL" \
    --store "$STORE" >"$WORKDIR/proxy.log" 2>"$WORKDIR/proxy.err" &
  PROXY_PID=$!
  PIDS+=("$PROXY_PID")
  wait_for_line "$WORKDIR/proxy.log" "proxying"
  PROXY_URL="$(sed -n 's/^idemgate .* proxying \(http:[^ ]*\) ->.*/\1/p' "$WORKDIR/proxy.log")"
  [ -n "$PROXY_URL" ] || fail "could not parse proxy address from banner"
}

processed() {
  "${CURL[@]}" "$PROXY_URL/processed" | sed -n 's/.*"processed":\([0-9]*\).*/\1/p'
}

echo "[1/10] build the proxy and the example backend"
(cd "$ROOT" && go build -o "$WORKDIR/idemgate" ./cmd/idemgate) || fail "proxy build failed"
(cd "$ROOT" && go build -o "$WORKDIR/backend" ./examples/backend) || fail "backend build failed"

echo "[2/10] --version matches the manifest version"
VERSION_OUT="$("$WORKDIR/idemgate" --version)"
[ "$VERSION_OUT" = "idemgate 0.1.0" ] || fail "unexpected version output: $VERSION_OUT"

echo "[3/10] start backend and proxy on loopback"
"$WORKDIR/backend" --listen 127.0.0.1:0 >"$WORKDIR/backend.log" 2>&1 &
PIDS+=($!)
wait_for_line "$WORKDIR/backend.log" "backend listening"
BACKEND_URL="$(sed -n 's/^backend listening on //p' "$WORKDIR/backend.log")"
[ -n "$BACKEND_URL" ] || fail "could not parse backend address"
start_proxy

echo "[4/10] first keyed POST executes on the backend"
R1="$("${CURL[@]}" -D "$WORKDIR/h1" -H 'Idempotency-Key: order-42' \
  -H 'Content-Type: application/json' -d '{"amount":1999,"currency":"usd"}' \
  "$PROXY_URL/charges")"
grep -q " 201" "$WORKDIR/h1" || fail "expected 201, headers: $(cat "$WORKDIR/h1")"
echo "$R1" | grep -q '"id":"ch_1"' || fail "unexpected first response: $R1"

echo "[5/10] duplicate replays without touching the backend"
R2="$("${CURL[@]}" -D "$WORKDIR/h2" -H 'Idempotency-Key: order-42' \
  -H 'Content-Type: application/json' -d '{"amount":1999,"currency":"usd"}' \
  "$PROXY_URL/charges")"
[ "$R1" = "$R2" ] || fail "replay body differs: $R2"
grep -qi '^idempotency-replayed: true' "$WORKDIR/h2" || fail "replay not marked"
[ "$(processed)" = "1" ] || fail "backend executed more than once"

echo "[6/10] same key with a different payload is refused (422)"
CODE="$("${CURL[@]}" -o /dev/null -w '%{http_code}' -H 'Idempotency-Key: order-42' \
  -H 'Content-Type: application/json' -d '{"amount":9999,"currency":"usd"}' \
  "$PROXY_URL/charges")"
[ "$CODE" = "422" ] || fail "expected 422 on key reuse, got $CODE"

echo "[7/10] keyless requests pass through undeduplicated"
"${CURL[@]}" -o /dev/null -H 'Content-Type: application/json' \
  -d '{"amount":500,"currency":"usd"}' "$PROXY_URL/charges"
"${CURL[@]}" -o /dev/null -H 'Content-Type: application/json' \
  -d '{"amount":500,"currency":"usd"}' "$PROXY_URL/charges"
[ "$(processed)" = "3" ] || fail "keyless requests were deduplicated"

echo "[8/10] records survive a proxy restart (file-backed store)"
kill "$PROXY_PID"
wait "$PROXY_PID" 2>/dev/null || true
start_proxy
R3="$("${CURL[@]}" -D "$WORKDIR/h3" -H 'Idempotency-Key: order-42' \
  -H 'Content-Type: application/json' -d '{"amount":1999,"currency":"usd"}' \
  "$PROXY_URL/charges")"
[ "$R3" = "$R1" ] || fail "post-restart replay differs: $R3"
grep -qi '^idempotency-replayed: true' "$WORKDIR/h3" || fail "post-restart replay not marked"
[ "$(processed)" = "3" ] || fail "restart re-executed a stored request"

echo "[9/10] ls shows the record; rm clears it and the key re-executes"
# Capture first, grep second: piping straight into 'grep -q' can close the
# pipe before the tool's final line, and pipefail would report the SIGPIPE
# as a failure.
LS_OUT="$("$WORKDIR/idemgate" ls --store "$STORE")"
echo "$LS_OUT" | grep -q "order-42" || fail "ls missing record: $LS_OUT"
RM_OUT="$("$WORKDIR/idemgate" rm --store "$STORE" order-42)"
echo "$RM_OUT" | grep -q "removed order-42" || fail "rm did not confirm: $RM_OUT"
"${CURL[@]}" -o /dev/null -H 'Idempotency-Key: order-42' \
  -H 'Content-Type: application/json' -d '{"amount":1999,"currency":"usd"}' \
  "$PROXY_URL/charges"
[ "$(processed)" = "4" ] || fail "key did not re-execute after rm"

echo "[10/10] purge reports its sweep"
PURGE_OUT="$("$WORKDIR/idemgate" purge --store "$STORE")"
echo "$PURGE_OUT" | grep -q "purged 0 expired" || fail "purge output unexpected: $PURGE_OUT"

echo "SMOKE OK"
