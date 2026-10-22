// Command beacon tells a hub what's happening on this box: components (state), runs and
// events, served read-only. See SPEC.md for the data and README.md for setup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eemax/beacon/internal/collect"
	"github.com/eemax/beacon/internal/logbook"
	"github.com/eemax/beacon/internal/server"
	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/statefile"
	"github.com/eemax/beacon/internal/tokens"
)

// Set at build time: -ldflags "-X main.version=v2.0.0".
var version = "dev"

const usage = `beacon: what's happening on this box, served read-only to a hub (lookout).

Usage:
  beacon collect                         update components and log new runs (every minute)
  beacon exec <component> [--trigger T] -- <command> [args]
                                         run a command and log it as a run of <component>
  beacon event deploy|config|note --title T [--component C] [--detail D]
                                         log an event (reboots are detected by collect)
  beacon run <component> --json FILE|-   log a finished run from a JSON document
  beacon put <component> [flags]         write a component's state by hand (ad-hoc)
  beacon serve                           serve state and log (read-only HTTP API)
  beacon log [--after N] [--limit N]     print log entries
  beacon token add|list|revoke [NAME]    tokens that may read this box
  beacon validate                        check components, definitions and the log
  beacon version

Where things live (environment; systemd units read /etc/beacon/beacon.env):
  BEACON_DATA        state, log, components   (default /var/lib/beacon)
  BEACON_ETC         components.d, tokens     (default /etc/beacon)
  BEACON_LISTEN      serve address            (default 127.0.0.1:7600)
  BEACON_SOURCE      this box's name          (default: the hostname)
  BEACON_HOST_FEED   host component on/off    (default on)

Run "beacon <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	commands := map[string]func([]string) error{
		"collect": cmdCollect, "exec": cmdExec, "event": cmdEvent, "run": cmdRun, "put": cmdPut,
		"serve": cmdServe, "log": cmdLog, "token": cmdToken, "validate": cmdValidate,
	}
	switch cmd {
	case "version", "--version":
		fmt.Println("beacon", version)
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	}
	fn, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "beacon: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err := fn(args); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}
		fmt.Fprintln(os.Stderr, "beacon:", err)
		var usageErr usageError
		if errors.As(err, &usageErr) || errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// exitError makes beacon exit with a child's exit code (beacon exec).
type exitError int

func (e exitError) Error() string { return "exit " + strconv.Itoa(int(e)) }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type paths struct{ data, etc string }

func (p paths) components() string  { return filepath.Join(p.data, "components") }
func (p paths) log() string         { return filepath.Join(p.data, "log.jsonl") }
func (p paths) state() string       { return filepath.Join(p.data, "state.json") }
func (p paths) definitions() string { return filepath.Join(p.etc, "components.d") }
func (p paths) tokens() string      { return env("BEACON_TOKENS", filepath.Join(p.etc, "tokens")) }

func newFlags(name string) (*flag.FlagSet, *paths) {
	fs := flag.NewFlagSet("beacon "+name, flag.ContinueOnError)
	p := &paths{}
	fs.StringVar(&p.data, "data", env("BEACON_DATA", "/var/lib/beacon"), "state, log and components folder")
	fs.StringVar(&p.etc, "etc", env("BEACON_ETC", "/etc/beacon"), "components.d and tokens folder")
	return fs, p
}

func collectConfig(p *paths, timeout time.Duration) collect.Config {
	return collect.Config{
		Components: p.components(), Definitions: p.definitions(), Log: p.log(), State: p.state(),
		HostFeed: env("BEACON_HOST_FEED", "on") != "off", Timeout: timeout, Now: time.Now,
	}
}

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ", ") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// ---------------------------------------------------------------- collect

func cmdCollect(args []string) error {
	fs, p := newFlags("collect")
	timeout := fs.Duration("timeout", 30*time.Second, "time limit per adapter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := collect.Run(context.Background(), collectConfig(p, *timeout))
	if err != nil {
		return err
	}
	for _, line := range res.Lines {
		fmt.Println(line)
	}
	if res.Failed > 0 {
		return fmt.Errorf("%d problem(s)", res.Failed)
	}
	return nil
}

// ---------------------------------------------------------------- exec

const tailBytes = 4000

// tail keeps the last bytes written to it (a command's output, for a failed run's error).
// stdout and stderr are copied in by two goroutines, hence the lock.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > tailBytes {
		t.buf = t.buf[len(t.buf)-tailBytes:]
	}
	return len(b), nil
}

func cmdExec(args []string) error {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 1 || sep == len(args)-1 {
		return usageError{"usage: beacon exec <component> [--trigger T] -- <command> [args]"}
	}
	component, flags, argv := args[0], args[1:sep], args[sep+1:]
	fs, p := newFlags("exec " + component)
	trigger := fs.String("trigger", "manual", "timer, manual, boot, dependency or other")
	timeout := fs.Duration("timeout", 30*time.Second, "time limit for the run adapter")
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if !spec.ValidName(component) || !spec.Triggers[*trigger] {
		return usageError{"beacon exec: bad component name or trigger"}
	}
	cfg := collectConfig(p, *timeout)
	defs, _ := collect.Definitions(cfg.Definitions)
	if err := os.MkdirAll(p.data, 0o755); err != nil {
		return err
	}
	id, start := collect.NewID(), time.Now().UTC().Truncate(time.Second)
	if err := statefile.Update(cfg.State, func(s *statefile.State) error {
		s.Running[id] = statefile.Marker{Component: component, StartedAt: start, Trigger: *trigger, PID: os.Getpid()}
		return nil
	}); err != nil {
		return err
	}

	out := &tail{}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, io.MultiWriter(os.Stdout, out), io.MultiWriter(os.Stderr, out)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stopped := make(chan bool, 1)
	go func() {
		for s := range sigs {
			select {
			case stopped <- true:
			default:
			}
			if cmd.Process != nil {
				cmd.Process.Signal(s) // the command decides how to stop
			}
		}
	}()
	err := cmd.Run()
	signal.Stop(sigs)
	close(sigs)
	wasStopped := len(stopped) > 0

	run := &spec.Run{Type: "run", Component: component, ID: id, StartedAt: start, EndedAt: time.Now().UTC().Truncate(time.Second),
		Trigger: *trigger, Unit: truncate(strings.Join(argv, " "), 200)}
	code := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		run.Outcome = spec.Success
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		run.Outcome = spec.Failure
		if wasStopped || code == 129 || code == 130 || code == 143 {
			run.Outcome = spec.Cancelled
		}
	default: // couldn't start it
		code = 127
		run.Outcome = spec.Failure
		out.Write([]byte(err.Error()))
	}
	run.ExitCode = &code
	if run.Outcome != spec.Success {
		run.Error = strings.TrimSpace(string(out.buf))
		if len(run.Error) > 2000 {
			run.Error = run.Error[len(run.Error)-2000:]
		}
	}
	if ferr := collect.FinishRun(context.Background(), cfg, defs[component], run); ferr != nil {
		fmt.Fprintln(os.Stderr, "beacon: logging the run:", ferr)
	}
	if code != 0 {
		return exitError(code)
	}
	return nil
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// ---------------------------------------------------------------- event, run

func cmdEvent(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageError{"usage: beacon event deploy|config|note --title T [--component C] [--detail D]"}
	}
	fs, p := newFlags("event " + args[0])
	title := fs.String("title", "", "what happened, in a few words")
	component := fs.String("component", "", "the component it's about (default: the whole box)")
	detail := fs.String("detail", "", "more about it")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	kind := spec.EventKind(args[0])
	if kind == "reboot" {
		return usageError{"reboots are logged by beacon collect"}
	}
	e := &spec.Event{Type: "event", ID: collect.NewID(), At: time.Now().UTC().Truncate(time.Second), Kind: kind, Component: *component, Title: *title, Detail: *detail}
	if err := os.MkdirAll(p.data, 0o755); err != nil {
		return err
	}
	_, err := logbook.Log{Path: p.log()}.Append(e)
	return err
}

func cmdRun(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageError{"usage: beacon run <component> --json FILE|-"}
	}
	fs, p := newFlags("run " + args[0])
	from := fs.String("json", "", "the run document (- for stdin)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *from == "" {
		return usageError{"beacon run: --json is required"}
	}
	data, err := readInput(*from)
	if err != nil {
		return err
	}
	var r spec.Run
	if err := spec.Decode(data, &r); err != nil {
		return err
	}
	r.Type, r.Seq = "run", 0
	if r.Component == "" {
		r.Component = args[0]
	}
	if r.Component != args[0] {
		return fmt.Errorf("document says component %q, command says %q", r.Component, args[0])
	}
	if r.ID == "" {
		r.ID = collect.NewID()
	}
	if err := os.MkdirAll(p.data, 0o755); err != nil {
		return err
	}
	_, err = logbook.Log{Path: p.log()}.Append(&r)
	return err
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(os.Stdin, spec.MaxBytes+1))
	}
	return os.ReadFile(path)
}

// ---------------------------------------------------------------- put (ad-hoc state)

func cmdPut(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageError{"usage: beacon put <component> [flags] (or --json FILE|-)"}
	}
	id := args[0]
	fs, p := newFlags("put " + id)
	title := fs.String("title", "", "display name")
	group := fs.String("group", "", "heading to group it under")
	description := fs.String("description", "", "what it is, in a sentence")
	status := fs.String("status", "", "ok, warn, fail or unknown")
	summary := fs.String("summary", "", "one line about its state")
	every := fs.String("expect-every", "", "longest normal gap between updates (stale after)")
	fromJSON := fs.String("json", "", "read the whole document from FILE (- for stdin)")
	var gauges, checks, primary, links repeated
	fs.Var(&gauges, "gauge", "key[{name=value,…}]=value[:unit[:label]] (repeatable)")
	fs.Var(&checks, "check", "name[{name=value,…}]=status[:detail] (repeatable)")
	fs.Var(&primary, "primary", "mark this gauge key as a headline number (repeatable)")
	fs.Var(&links, "link", "title=url (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Second)
	var c *spec.Component
	if *fromJSON != "" {
		data, err := readInput(*fromJSON)
		if err != nil {
			return err
		}
		c = &spec.Component{}
		if err := spec.Decode(data, c); err != nil {
			return err
		}
		if c.ID == "" {
			c.ID = id
		}
		if c.UpdatedAt.IsZero() {
			c.UpdatedAt = now
		}
		if c.ID != id {
			return fmt.Errorf("document says id %q, command says %q", c.ID, id)
		}
	} else {
		c = &spec.Component{ID: id, Title: *title, Group: *group, Description: *description, Status: spec.Status(*status),
			Summary: *summary, UpdatedAt: now, ExpectEvery: *every}
		for _, g := range gauges {
			m, err := parseMetric(g)
			if err != nil {
				return err
			}
			c.Gauges = append(c.Gauges, m)
		}
		for _, k := range checks {
			ck, err := parseCheck(k)
			if err != nil {
				return err
			}
			c.Checks = append(c.Checks, ck)
		}
		for _, l := range links {
			t, u, ok := strings.Cut(l, "=")
			if !ok {
				return fmt.Errorf("--link %q: want title=url", l)
			}
			c.Links = append(c.Links, spec.Link{Title: t, URL: u})
		}
		for _, key := range primary {
			found := false
			for i := range c.Gauges {
				if c.Gauges[i].Key == key {
					c.Gauges[i].Primary, found = true, true
				}
			}
			if !found {
				return fmt.Errorf("--primary %s: no such gauge", key)
			}
		}
	}
	if err := os.MkdirAll(p.components(), 0o755); err != nil {
		return err
	}
	return spec.WriteComponent(p.components(), c)
}

// splitLabels splits "key{a=b,c=d}" into the key and its labels.
func splitLabels(s string) (string, spec.Labels, error) {
	key, rest, ok := strings.Cut(s, "{")
	if !ok {
		return s, nil, nil
	}
	body, ok := strings.CutSuffix(rest, "}")
	if !ok {
		return "", nil, fmt.Errorf("%q: labels must end with }", s)
	}
	labels := spec.Labels{}
	for _, pair := range strings.Split(body, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return "", nil, fmt.Errorf("%q: labels are name=value pairs", s)
		}
		labels[k] = v
	}
	return key, labels, nil
}

// cutTarget splits "key{labels}=rest" at the "=" after any labels.
func cutTarget(s string) (string, string, bool) {
	start := 0
	if i := strings.Index(s, "}"); i >= 0 {
		start = i
	}
	i := strings.Index(s[start:], "=")
	if i < 0 {
		return "", "", false
	}
	return s[:start+i], s[start+i+1:], true
}

func parseMetric(s string) (spec.Metric, error) {
	target, rest, ok := cutTarget(s)
	if !ok {
		return spec.Metric{}, fmt.Errorf("--gauge %q: want key[{labels}]=value[:unit[:label]]", s)
	}
	key, labels, err := splitLabels(target)
	if err != nil {
		return spec.Metric{}, fmt.Errorf("--gauge %w", err)
	}
	parts := strings.SplitN(rest, ":", 3)
	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return spec.Metric{}, fmt.Errorf("--gauge %s: %q is not a number", key, parts[0])
	}
	m := spec.Metric{Key: key, Value: value, Labels: labels}
	if len(parts) > 1 {
		m.Unit = parts[1]
	}
	if len(parts) > 2 {
		m.Label = parts[2]
	}
	return m, nil
}

func parseCheck(s string) (spec.Check, error) {
	target, rest, ok := cutTarget(s)
	if !ok {
		return spec.Check{}, fmt.Errorf("--check %q: want name[{labels}]=status[:detail]", s)
	}
	name, labels, err := splitLabels(target)
	if err != nil {
		return spec.Check{}, fmt.Errorf("--check %w", err)
	}
	status, detail, _ := strings.Cut(rest, ":")
	return spec.Check{Name: name, Status: spec.Status(status), Detail: detail, Labels: labels}, nil
}

// ---------------------------------------------------------------- serve

func cmdServe(args []string) error {
	fs, p := newFlags("serve")
	listen := fs.String("listen", env("BEACON_LISTEN", "127.0.0.1:7600"), "address to listen on")
	hostname, _ := os.Hostname()
	source := fs.String("source", env("BEACON_SOURCE", hostname), "this box's name, as the hub knows it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := tokens.Open(p.tokens())
	if err := store.Err(); err != nil {
		log.Warn("no usable tokens yet: every request will be refused", "file", p.tokens(), "err", err)
	}
	srv := &server.Server{Components: p.components(), Definitions: p.definitions(), LogPath: p.log(), Source: *source, Version: version, Tokens: store, Log: log, Now: time.Now}
	httpServer := &http.Server{
		Addr: *listen, Handler: srv.Handler(), MaxHeaderBytes: 16 << 10,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdown)
	}()
	log.Info("serving", "listen", *listen, "data", p.data, "source", *source, "version", version)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- log, token, validate

func cmdLog(args []string) error {
	fs, p := newFlags("log")
	after := fs.Int64("after", 0, "entries after this seq")
	limit := fs.Int("limit", 50, "at most this many (the newest, unless --after)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l := logbook.Log{Path: p.log()}
	from := *after
	if from == 0 {
		_, _, last, err := l.Read(1<<62, 0)
		if err != nil {
			return err
		}
		from = max(0, last-int64(*limit))
	}
	entries, _, _, err := l.Read(from, *limit)
	for _, e := range entries {
		fmt.Println(string(e))
	}
	return err
}

func cmdToken(args []string) error {
	if len(args) == 0 {
		return usageError{"usage: beacon token add|list|revoke [NAME]"}
	}
	action := args[0]
	fs, p := newFlags("token")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch {
	case action == "list" && fs.NArg() == 0:
		names, err := tokens.Names(p.tokens())
		for _, n := range names {
			fmt.Println(n)
		}
		return err
	case action == "add" && fs.NArg() == 1:
		token, err := tokens.Add(p.tokens(), fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Println(token) // only shown once: redirect it straight to where it's needed
		return nil
	case action == "revoke" && fs.NArg() == 1:
		return tokens.Revoke(p.tokens(), fs.Arg(0))
	}
	return usageError{"usage: beacon token add|list|revoke [NAME]"}
}

func cmdValidate(args []string) error {
	fs, p := newFlags("validate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bad := 0
	comps, invalid, err := spec.LoadComponents(p.components())
	if err != nil {
		return err
	}
	for _, c := range comps {
		fmt.Printf("ok   component %s\n", c.ID)
	}
	defs, defInvalid := collect.Definitions(p.definitions())
	for id := range defs {
		fmt.Printf("ok   definition %s\n", id)
	}
	for _, i := range append(invalid, defInvalid...) {
		bad++
		fmt.Printf("BAD  %s: %s\n", i.File, i.Error)
	}
	entries, first, last, err := logbook.Log{Path: p.log()}.Read(0, 1<<30)
	if err != nil {
		return err
	}
	for _, raw := range entries {
		var head struct{ Type string }
		json.Unmarshal(raw, &head)
		var e interface{ Validate() error }
		if head.Type == "run" {
			e = &spec.Run{}
		} else {
			e = &spec.Event{}
		}
		if err := json.Unmarshal(raw, e); err != nil || e.Validate() != nil {
			bad++
			fmt.Printf("BAD  log entry %s\n", truncate(string(raw), 120))
		}
	}
	fmt.Printf("log: %d entries (seq %d-%d)\n", len(entries), first, last)
	if bad > 0 {
		return fmt.Errorf("%d invalid", bad)
	}
	return nil
}
