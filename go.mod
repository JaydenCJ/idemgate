// idemgate — a drop-in reverse proxy that adds Idempotency-Key
// deduplication to any HTTP backend, with a file-backed store.
//
// version:    0.1.0
// author:     JaydenCJ
// license:    MIT
// repository: https://github.com/JaydenCJ/idemgate
// keywords:   idempotency, reverse-proxy, deduplication, http, retries, double-charge, api-safety
//
// Zero runtime dependencies: the require list below is intentionally empty
// and must stay that way (see CONTRIBUTING.md, "Ground rules").
module github.com/JaydenCJ/idemgate

go 1.22
