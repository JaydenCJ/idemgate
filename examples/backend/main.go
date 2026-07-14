// Command backend is a deliberately non-idempotent toy payments API used
// by the idemgate examples and smoke test. Every POST /charges creates a
// brand-new charge — exactly the behavior that double-bills a customer
// when a client retries. Put idemgate in front of it and retries with the
// same Idempotency-Key stop reaching it.
//
// Endpoints:
//
//	POST /charges    create a charge; 201 with a fresh id every call
//	GET  /processed  how many charges this backend really executed
//
// It binds loopback and prints its address on stdout so scripts can use
// --listen 127.0.0.1:0.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
)

type charge struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "bind address")
	flag.Parse()

	var (
		mu        sync.Mutex
		processed int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /charges", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		processed++
		id := processed
		mu.Unlock()

		c := charge{
			ID:       fmt.Sprintf("ch_%d", id),
			Amount:   req.Amount,
			Currency: req.Currency,
			Status:   "succeeded",
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/charges/"+c.ID)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("GET /processed", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := processed
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"processed\":%d}\n", n)
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("backend: %v", err)
	}
	fmt.Printf("backend listening on http://%s\n", ln.Addr())
	log.Fatal(http.Serve(ln, mux))
}
