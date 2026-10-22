// Command beacon publishes status feeds from a box and serves them, read-only, to a hub.
// See SPEC.md for the feed format and README.md for setup.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eemax/beacon/internal/collect"
	"github.com/eemax/beacon/internal/feed"
	"github.com/eemax/beacon/internal/host"
	"github.com/eemax/beacon/internal/server"
	"github.com/eemax/beacon/internal/tokens"
)

// Set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `beacon: status feeds from this box, served read-only to a hub.

Usage:
  beacon put <feed> --status ok|warn|fail|unknown [flags]   write a feed
  beacon put <feed> --json FILE|-                           write a feed from a JSON document
  beacon collect                     run the host collector and collectors.d/* once
  beacon serve                       serve the feeds folder (read-only HTTP API)
  beacon token add|list|revoke [NAME]  manage the tokens that may read feeds
  beacon validate [FILE ...]         check feed files (default: the whole feeds folder)
  beacon version

Settings (flags override the environment; systemd units read /etc/beacon/beacon.env):
  BEACON_FEEDS       feeds folder           (default /var/lib/beacon/feeds)
  BEACON_TOKENS      tokens file            (default /etc/beacon/tokens)
  BEACON_COLLECTORS  collectors folder      (default /etc/beacon/collectors.d)
  BEACON_LISTEN      serve address          (default 127.0.0.1:7600)
  BEACON_SOURCE      this box's name        (default: the hostname)
  BEACON_HOST_FEED   host collector on/off  (default on, Linux only)

Run "beacon <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "put":
		err = cmdPut(args)
	case "collect":
		err = cmdCollect(args)
	case "serve":
		err = cmdServe(args)
	case "token":
		err = cmdToken(args)
	case "validate":
		err = cmdValidate(args)
	case "version", "--version":
		fmt.Println("beacon", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "beacon: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		var usageErr usageError
		fmt.Fprintln(os.Stderr, "beacon:", err)
		if errors.As(err, &usageErr) || errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("beacon "+name, flag.ContinueOnError)
	feeds := fs.String("feeds", env("BEACON_FEEDS", "/var/lib/beacon/feeds"), "feeds folder")
	return fs, feeds
}

// ---------------------------------------------------------------- put

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ", ") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func cmdPut(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageError{"usage: beacon put <feed> --status … (or --json FILE|-)"}
	}
	name := args[0]
	fs, feeds := newFlags("put " + name)
	status := fs.String("status", "", "ok, warn, fail or unknown")
	title := fs.String("title", "", "display name")
	summary := fs.String("summary", "", "one line for people")
	every := fs.String("expect-every", "", "longest normal gap between updates (e.g. 26h); stale after")
	public := fs.Bool("public", false, "show this feed on the hub's public page")
	fromJSON := fs.String("json", "", "read the whole document from FILE (- for stdin)")
	var metrics, checks, publicMetrics repeated
	fs.Var(&metrics, "metric", "key=value[:unit[:label]] (repeatable)")
	fs.Var(&checks, "check", "name=status[:detail] (repeatable)")
	fs.Var(&publicMetrics, "public-metric", "show this metric key publicly (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("unexpected arguments: %v", fs.Args())}
	}

	var f *feed.Feed
	if *fromJSON != "" {
		// The document is the whole feed: other flags would be silently ignored, so refuse them.
		var extra []string
		fs.Visit(func(fl *flag.Flag) {
			if fl.Name != "json" && fl.Name != "feeds" && fl.Name != "public-metric" {
				extra = append(extra, "--"+fl.Name)
			}
		})
		if len(extra) > 0 {
			return usageError{fmt.Sprintf("--json takes the whole feed from the document; drop %s", strings.Join(extra, " "))}
		}
		data, err := readInput(*fromJSON)
		if err != nil {
			return err
		}
		if f, err = collect.FromJSON(data, name, time.Now); err != nil {
			return err
		}
		if f.Feed != name {
			return fmt.Errorf("document says feed %q, command says %q", f.Feed, name)
		}
	} else {
		f = &feed.Feed{
			Schema: feed.Schema, Feed: name, Title: *title, Status: feed.Status(*status),
			Summary: *summary, UpdatedAt: time.Now().UTC().Truncate(time.Second),
			ExpectEvery: *every,
		}
		if *public {
			f.Visibility = feed.Public
		}
		for _, m := range metrics {
			metric, err := parseMetric(m)
			if err != nil {
				return err
			}
			f.Metrics = append(f.Metrics, metric)
		}
		for _, c := range checks {
			check, err := parseCheck(c)
			if err != nil {
				return err
			}
			f.Checks = append(f.Checks, check)
		}
	}
	for _, key := range publicMetrics {
		found := false
		for i := range f.Metrics {
			if f.Metrics[i].Key == key {
				f.Metrics[i].Visibility, found = feed.Public, true
			}
		}
		if !found {
			return fmt.Errorf("--public-metric %s: no such metric", key)
		}
	}
	return feed.Write(*feeds, f)
}

func parseMetric(s string) (feed.Metric, error) {
	key, rest, ok := strings.Cut(s, "=")
	if !ok {
		return feed.Metric{}, fmt.Errorf("--metric %q: want key=value[:unit[:label]]", s)
	}
	parts := strings.SplitN(rest, ":", 3)
	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return feed.Metric{}, fmt.Errorf("--metric %s: %q is not a number", key, parts[0])
	}
	m := feed.Metric{Key: key, Value: value}
	if len(parts) > 1 {
		m.Unit = parts[1]
	}
	if len(parts) > 2 {
		m.Label = parts[2]
	}
	return m, nil
}

func parseCheck(s string) (feed.Check, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok {
		return feed.Check{}, fmt.Errorf("--check %q: want name=status[:detail]", s)
	}
	status, detail, _ := strings.Cut(rest, ":")
	return feed.Check{Name: name, Status: feed.Status(status), Detail: detail}, nil
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(os.Stdin, feed.MaxBytes+1))
	}
	return os.ReadFile(path)
}

// ---------------------------------------------------------------- collect

func cmdCollect(args []string) error {
	fs, feeds := newFlags("collect")
	dir := fs.String("collectors", env("BEACON_COLLECTORS", "/etc/beacon/collectors.d"), "collectors folder")
	timeout := fs.Duration("timeout", 30*time.Second, "time limit per collector")
	hostFeed := fs.Bool("host", env("BEACON_HOST_FEED", "on") != "off", "write the built-in host feed (Linux)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var failed int
	if *hostFeed && host.Supported {
		f, err := host.Feed("/", time.Now())
		if err == nil {
			err = feed.Write(*feeds, f)
		}
		report("host (built-in)", "host", err, &failed)
	}
	results, err := collect.Run(context.Background(), *dir, *feeds, *timeout, time.Now)
	if err != nil {
		return err
	}
	for _, r := range results {
		report(r.Collector, r.Feed, r.Err, &failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d collector(s) failed", failed)
	}
	return nil
}

func report(collector, feedName string, err error, failed *int) {
	if err != nil {
		*failed++
		fmt.Fprintf(os.Stderr, "%s: %v\n", collector, err)
		return
	}
	fmt.Printf("%s -> %s\n", collector, feedName)
}

// ---------------------------------------------------------------- serve

func cmdServe(args []string) error {
	fs, feeds := newFlags("serve")
	listen := fs.String("listen", env("BEACON_LISTEN", "127.0.0.1:7600"), "address to listen on")
	tokensPath := fs.String("tokens", env("BEACON_TOKENS", "/etc/beacon/tokens"), "tokens file")
	hostname, _ := os.Hostname()
	source := fs.String("source", env("BEACON_SOURCE", hostname), "this box's name, as the hub knows it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if info, err := os.Stat(*feeds); err != nil || !info.IsDir() {
		return fmt.Errorf("feeds folder %s: not a folder (create it, or set BEACON_FEEDS)", *feeds)
	}
	store := tokens.Open(*tokensPath)
	if err := store.Err(); err != nil {
		log.Warn("no usable tokens yet: every feed request will be refused", "file", *tokensPath, "err", err)
	}
	srv := &server.Server{Dir: *feeds, Source: *source, Version: version, Tokens: store, Log: log, Now: time.Now}
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdown)
	}()
	log.Info("serving feeds", "listen", *listen, "feeds", *feeds, "source", *source, "version", version)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- token

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("beacon token", flag.ContinueOnError)
	path := fs.String("tokens", env("BEACON_TOKENS", "/etc/beacon/tokens"), "tokens file")
	if len(args) == 0 {
		return usageError{"usage: beacon token add|list|revoke [NAME]"}
	}
	action := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch {
	case action == "list" && fs.NArg() == 0:
		names, err := tokens.Names(*path)
		for _, n := range names {
			fmt.Println(n)
		}
		return err
	case action == "add" && fs.NArg() == 1:
		token, err := tokens.Add(*path, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Println(token) // only shown once: redirect it straight into a file
		return nil
	case action == "revoke" && fs.NArg() == 1:
		return tokens.Revoke(*path, fs.Arg(0))
	}
	return usageError{"usage: beacon token add|list|revoke [NAME]"}
}

// ---------------------------------------------------------------- validate

func cmdValidate(args []string) error {
	fs, feeds := newFlags("validate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bad := 0
	if fs.NArg() == 0 {
		found, invalid, err := feed.LoadAll(*feeds)
		if err != nil {
			return err
		}
		for _, f := range found {
			fmt.Printf("ok   %s.json\n", f.Feed)
		}
		for _, inv := range invalid {
			bad++
			fmt.Printf("BAD  %s: %s\n", inv.File, inv.Error)
		}
	} else {
		for _, path := range fs.Args() {
			data, err := os.ReadFile(path)
			if err == nil {
				var f *feed.Feed
				if f, err = feed.Parse(data); err == nil && f.Feed+".json" != filepath.Base(path) {
					err = fmt.Errorf("feed %q doesn't match the file name", f.Feed)
				}
			}
			if err != nil {
				bad++
				fmt.Printf("BAD  %s: %v\n", path, err)
			} else {
				fmt.Printf("ok   %s\n", path)
			}
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d invalid", bad)
	}
	return nil
}
