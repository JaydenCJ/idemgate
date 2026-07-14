// Package config parses and validates the serve command's configuration.
//
// Every knob has a safe default: bind loopback, gate POST only, keep
// records for 24 hours, cap buffered bodies. The only required flag is
// --upstream, because guessing where to send someone's payment traffic
// is not a default worth having.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JaydenCJ/idemgate/internal/policy"
)

// Config is the fully validated serve configuration.
type Config struct {
	Listen       string           // bind address for the proxy listener
	Upstream     *url.URL         // backend origin (scheme + host [+ path prefix])
	StoreDir     string           // record/lease directory
	TTL          time.Duration    // record retention
	LeaseTimeout time.Duration    // staleness bound for crashed in-flight leases
	Methods      policy.MethodSet // methods that go through the gate
	HeaderName   string           // idempotency header name
	RequireKey   bool             // reject gated requests without a key
	MaxRequest   int64            // request-body cap for keyed requests (bytes)
	MaxResponse  int64            // stored-response cap (bytes)
}

// Default returns the configuration before flags are applied. Upstream is
// nil and must be supplied.
func Default() *Config {
	return &Config{
		Listen:       "127.0.0.1:8080",
		StoreDir:     ".idemgate",
		TTL:          24 * time.Hour,
		LeaseTimeout: 30 * time.Second,
		Methods:      policy.MethodSet{"POST": true},
		HeaderName:   "Idempotency-Key",
		MaxRequest:   1 << 20, // 1 MiB
		MaxResponse:  8 << 20, // 8 MiB
	}
}

// ParseServe parses serve's flags into a validated Config.
func ParseServe(args []string) (*Config, error) {
	cfg := Default()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "bind address")
	upstream := fs.String("upstream", "", "backend origin URL (required)")
	fs.StringVar(&cfg.StoreDir, "store", cfg.StoreDir, "record directory")
	fs.DurationVar(&cfg.TTL, "ttl", cfg.TTL, "record retention")
	fs.DurationVar(&cfg.LeaseTimeout, "lease-timeout", cfg.LeaseTimeout, "in-flight lease staleness bound")
	methods := fs.String("methods", "POST", "comma-separated methods to gate")
	fs.StringVar(&cfg.HeaderName, "header", cfg.HeaderName, "idempotency header name")
	fs.BoolVar(&cfg.RequireKey, "require-key", false, "reject gated requests without a key")
	maxRequest := fs.String("max-request", "1MiB", "request-body cap for keyed requests")
	maxResponse := fs.String("max-response", "8MiB", "stored-response cap")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q after serve flags", fs.Arg(0))
	}

	var err error
	if cfg.Upstream, err = parseUpstream(*upstream); err != nil {
		return nil, err
	}
	if cfg.Methods, err = policy.ParseMethods(*methods); err != nil {
		return nil, err
	}
	if cfg.MaxRequest, err = ParseSize(*maxRequest); err != nil {
		return nil, fmt.Errorf("--max-request: %w", err)
	}
	if cfg.MaxResponse, err = ParseSize(*maxResponse); err != nil {
		return nil, fmt.Errorf("--max-response: %w", err)
	}
	if cfg.TTL <= 0 {
		return nil, fmt.Errorf("--ttl must be positive, got %s", cfg.TTL)
	}
	if cfg.LeaseTimeout <= 0 {
		return nil, fmt.Errorf("--lease-timeout must be positive, got %s", cfg.LeaseTimeout)
	}
	if !validHeaderName(cfg.HeaderName) {
		return nil, fmt.Errorf("--header %q is not a valid HTTP header name", cfg.HeaderName)
	}
	return cfg, nil
}

func parseUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("--upstream is required (e.g. --upstream http://127.0.0.1:9000)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("--upstream: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--upstream scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--upstream %q has no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("--upstream must not carry a query or fragment (a path prefix is fine)")
	}
	if u.User != nil {
		return nil, errors.New("--upstream must not embed credentials")
	}
	return u, nil
}

// ParseSize parses a byte size: a plain integer, or one with a KiB, MiB
// or GiB suffix. Only binary suffixes are accepted, so "1MiB" is exactly
// 1048576 and there is no ambiguity about what a cap means.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "GiB"):
		mult, t = 1<<30, strings.TrimSuffix(t, "GiB")
	case strings.HasSuffix(t, "MiB"):
		mult, t = 1<<20, strings.TrimSuffix(t, "MiB")
	case strings.HasSuffix(t, "KiB"):
		mult, t = 1<<10, strings.TrimSuffix(t, "KiB")
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (use bytes or a KiB/MiB/GiB suffix, e.g. 512KiB)", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive, got %q", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return n * mult, nil
}

// validHeaderName checks RFC 9110 token syntax.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
