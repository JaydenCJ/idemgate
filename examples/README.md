# idemgate examples

This directory contains a deliberately **non-idempotent** toy payments
backend and a walkthrough script. Everything runs on loopback; nothing
touches the network.

## The backend

`backend/main.go` is the API you do not want to expose to retries: every
`POST /charges` mints a brand-new charge (`ch_1`, `ch_2`, …), and
`GET /processed` reports how many charges it really executed. It is the
same backend the smoke test uses.

```bash
go build -o backend ./backend
./backend --listen 127.0.0.1:9000
```

## 1. Watch the gate work

`demo.sh` builds both binaries, starts the backend and the proxy, and
narrates the full lifecycle:

```bash
bash examples/demo.sh
```

It sends the same keyed request twice (one execution, one replay), reuses
the key with a different amount (422), fires two keyless requests (both
execute), then prints the backend's `processed` count and the store
listing so you can see exactly what was deduplicated.

## 2. Poke it by hand

With the backend on `:9000` and the proxy on `:8080`
(`idemgate serve --upstream http://127.0.0.1:9000 --store ./gate-store`):

```bash
# Run this twice — the second response carries Idempotency-Replayed: true
curl -i -H 'Idempotency-Key: order-42' -H 'Content-Type: application/json' \
  -d '{"amount":1999,"currency":"usd"}' http://127.0.0.1:8080/charges

# Same key, different body: refused with 422
curl -i -H 'Idempotency-Key: order-42' -H 'Content-Type: application/json' \
  -d '{"amount":9999,"currency":"usd"}' http://127.0.0.1:8080/charges

# How many charges actually happened?
curl http://127.0.0.1:8080/processed
```

## 3. Administer the store

```bash
idemgate ls --store ./gate-store          # what is recorded, and until when
idemgate rm --store ./gate-store order-42 # forget one key (it will re-execute)
idemgate purge --store ./gate-store       # sweep expired records + stale leases
```

Records are plain files under `gate-store/records/` — open one. The
format is documented in [docs/gate-semantics.md](../docs/gate-semantics.md).
