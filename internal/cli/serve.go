// The serve command: bind, print the banner, run the gate until killed.
package cli

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/JaydenCJ/idemgate/internal/config"
	"github.com/JaydenCJ/idemgate/internal/proxy"
	"github.com/JaydenCJ/idemgate/internal/store"
	"github.com/JaydenCJ/idemgate/internal/version"
)

func cmdServe(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.ParseServe(args)
	if err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return exitError
	}
	st := store.New(cfg.StoreDir, cfg.TTL, cfg.LeaseTimeout)
	if err := st.Init(); err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return exitError
	}
	gate := proxy.New(cfg, st)
	gate.Logger = log.New(stderr, "", log.LstdFlags)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return exitError
	}
	// The banner is a stable, parseable line: the smoke test and process
	// managers read the bound address from it (important with :0).
	fmt.Fprintf(stdout, "idemgate %s proxying http://%s -> %s (store %s, ttl %s, methods %s)\n",
		version.Version, ln.Addr(), cfg.Upstream, cfg.StoreDir, cfg.TTL, cfg.Methods)

	srv := &http.Server{
		Handler:           gate,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return exitError
	}
	return exitOK
}
