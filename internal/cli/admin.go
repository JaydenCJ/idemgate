// Store administration commands: ls, rm, purge.
//
// These operate on the record directory directly and are safe to run
// while a proxy is serving from it — writes are atomic and rm also
// clears leases, so it can always unstick a wedged key.
package cli

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/JaydenCJ/idemgate/internal/store"
)

// adminDefaults mirror serve's defaults; ls/rm/purge only need the store
// path and the lease staleness bound (record expiry lives inside each
// record, not in configuration).
const adminLeaseTimeout = 30 * time.Second

func adminStore(fs *flag.FlagSet, args []string, stderr io.Writer) (*store.Store, []string, bool) {
	fs.SetOutput(io.Discard)
	dir := fs.String("store", defaultStore, "record directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return nil, nil, false
	}
	return store.New(*dir, 24*time.Hour, adminLeaseTimeout), fs.Args(), true
}

func cmdLs(args []string, stdout, stderr io.Writer) int {
	st, rest, ok := adminStore(flag.NewFlagSet("ls", flag.ContinueOnError), args, stderr)
	if !ok {
		return exitError
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "idemgate: ls takes no arguments, got %q\n", rest[0])
		return exitError
	}
	records, corrupt, err := st.List()
	if err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return exitError
	}
	for _, path := range corrupt {
		fmt.Fprintf(stderr, "idemgate: skipping corrupt record %s (clear it with 'idemgate rm')\n", path)
	}
	now := st.Now()
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSTATUS\tSTORED\tEXPIRES\tSTATE")
	for _, rec := range records {
		state := "live"
		if rec.Expired(now) {
			state = "expired"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
			rec.Key, rec.Status,
			rec.StoredAt.UTC().Format(time.RFC3339),
			rec.ExpiresAt.UTC().Format(time.RFC3339),
			state)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "%d record(s)\n", len(records))
	return exitOK
}

func cmdRm(args []string, stdout, stderr io.Writer) int {
	st, keys, ok := adminStore(flag.NewFlagSet("rm", flag.ContinueOnError), args, stderr)
	if !ok {
		return exitError
	}
	if len(keys) == 0 {
		fmt.Fprintln(stderr, "idemgate: rm needs at least one key")
		return exitError
	}
	code := exitOK
	for _, key := range keys {
		found, err := st.Remove(key)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "idemgate: %v\n", err)
			return exitError
		case found:
			fmt.Fprintf(stdout, "removed %s\n", key)
		default:
			fmt.Fprintf(stderr, "idemgate: no record for key %q\n", key)
			code = exitFailure
		}
	}
	return code
}

func cmdPurge(args []string, stdout, stderr io.Writer) int {
	st, rest, ok := adminStore(flag.NewFlagSet("purge", flag.ContinueOnError), args, stderr)
	if !ok {
		return exitError
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "idemgate: purge takes no arguments, got %q\n", rest[0])
		return exitError
	}
	stats, err := st.Purge()
	if err != nil {
		fmt.Fprintf(stderr, "idemgate: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "purged %d expired record(s); kept %d live; cleared %d stale lease(s)\n",
		stats.Removed, stats.Kept, stats.StaleLeases)
	if stats.Corrupt > 0 {
		fmt.Fprintf(stderr, "idemgate: %d corrupt record(s) left in place; inspect and clear them with 'idemgate rm'\n", stats.Corrupt)
	}
	return exitOK
}
