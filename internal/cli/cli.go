// Package cli implements the idemgate command-line interface.
//
// The entry point is Run, which takes argv and explicit streams and
// returns a process exit code. Keeping the CLI a pure function of its
// inputs (no os.Exit, no global state) lets the tests drive every
// subcommand in-process and deterministically.
//
// Exit codes:
//
//	0  success
//	1  operational failure (e.g. rm of a key that has no record)
//	2  usage, configuration or I/O error
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/JaydenCJ/idemgate/internal/version"
)

const (
	exitOK       = 0
	exitFailure  = 1
	exitError    = 2
	defaultStore = ".idemgate"
)

const usageText = `idemgate %s — Idempotency-Key deduplicating reverse proxy

Usage:
  idemgate <command> [flags]

Commands:
  serve    run the proxy in front of one upstream backend
  ls       list stored idempotency records
  rm       delete records (and any lease) by key
  purge    delete expired records and stale leases
  version  print the idemgate version

Serve flags:
  --upstream URL       backend origin, e.g. http://127.0.0.1:9000 (required)
  --listen ADDR        bind address (default 127.0.0.1:8080)
  --store DIR          record directory (default %s)
  --ttl DUR            record retention (default 24h)
  --methods LIST       comma-separated methods to gate (default POST)
  --header NAME        idempotency header name (default Idempotency-Key)
  --require-key        answer 400 when a gated request has no key
  --lease-timeout DUR  crashed in-flight lease staleness bound (default 30s)
  --max-request SIZE   request-body cap for keyed requests (default 1MiB)
  --max-response SIZE  stored-response cap (default 8MiB)

ls / rm / purge take --store DIR (default %s).

Exit codes: 0 ok, 1 operational failure, 2 usage/config/IO error.
`

// Run executes one idemgate invocation and returns its exit code.
func Run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		printUsage(stderr)
		return exitError
	}
	switch arg := argv[0]; {
	case arg == "--version" || arg == "-V" || arg == "version":
		fmt.Fprintf(stdout, "idemgate %s\n", version.Version)
		return exitOK
	case arg == "--help" || arg == "-h" || arg == "help":
		printUsage(stdout)
		return exitOK
	case strings.HasPrefix(arg, "-"):
		fmt.Fprintf(stderr, "idemgate: unknown global flag %q\n", arg)
		printUsage(stderr)
		return exitError
	case arg == "serve":
		return cmdServe(argv[1:], stdout, stderr)
	case arg == "ls":
		return cmdLs(argv[1:], stdout, stderr)
	case arg == "rm":
		return cmdRm(argv[1:], stdout, stderr)
	case arg == "purge":
		return cmdPurge(argv[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "idemgate: unknown command %q\n", arg)
		printUsage(stderr)
		return exitError
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, usageText, version.Version, defaultStore, defaultStore)
}
