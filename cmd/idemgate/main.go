// Command idemgate is a drop-in reverse proxy that adds Idempotency-Key
// deduplication to any HTTP backend. See README.md for the full story.
package main

import (
	"os"

	"github.com/JaydenCJ/idemgate/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
