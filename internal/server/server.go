// Package server is beacon's read-only HTTP API (SPEC.md → HTTP API): /v2/state (the
// components now, the box, the log's bounds) and /v2/log (runs and events after a seq).
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eemax/beacon/internal/collect"
	"github.com/eemax/beacon/internal/host"
	"github.com/eemax/beacon/internal/logbook"
	"github.com/eemax/beacon/internal/spec"
	"github.com/eemax/beacon/internal/tokens"
)

type Server struct {
	Components  string // component state files
	Definitions string // components.d (only to report broken definitions)
	LogPath     string
	Source      string // this box's name, as the hub knows it
	Version     string
	Tokens      *tokens.Store
	Log         *slog.Logger
	Now         func() time.Time
}

type bounds struct {
	First int64 `json:"first_seq"`
	Last  int64 `json:"last_seq"`
}

type state struct {
	Schema      int               `json:"schema"`
	Source      string            `json:"source"`
	Beacon      string            `json:"beacon"`
	GeneratedAt time.Time         `json:"generated_at"`
	Box         host.Facts        `json:"box"`
	Log         bounds            `json:"log"`
	Components  []*spec.Component `json:"components"`
	Invalid     []spec.Invalid    `json:"invalid"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v2/state", s.authed(s.state))
	mux.HandleFunc("GET /v2/log", s.authed(s.log))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	return readOnly(mux)
}

// readOnly answers 405 to anything but GET/HEAD before routing, so no path can ever
// accept a write.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		name, valid := "", false
		if ok {
			name, valid = s.Tokens.Check(strings.TrimSpace(token))
		}
		if !valid {
			s.Log.Warn("unauthorized", "path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="beacon"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		start, sw := time.Now(), &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next(sw, r)
		// One line per request: lets a hub's timeouts be told apart from slow answers.
		s.Log.Info("request", "token", name, "path", r.URL.Path, "query", r.URL.RawQuery, "status", sw.status, "ms", time.Since(start).Milliseconds())
	}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	comps, invalid, err := spec.LoadComponents(s.Components)
	if err != nil {
		s.Log.Error("reading components", "dir", s.Components, "err", err)
		http.Error(w, "can't read the components folder", http.StatusInternalServerError)
		return
	}
	_, defInvalid := collect.Definitions(s.Definitions)
	invalid = append(invalid, defInvalid...)
	_, first, last, err := logbook.Log{Path: s.LogPath}.Read(1<<62, 0)
	if err != nil {
		s.Log.Error("reading log", "err", err)
	}
	if comps == nil {
		comps = []*spec.Component{}
	}
	if invalid == nil {
		invalid = []spec.Invalid{}
	}
	body := state{Schema: spec.Schema, Source: s.Source, Beacon: s.Version, Box: host.ReadFacts(), Log: bounds{first, last}, Components: comps, Invalid: invalid}
	// The ETag covers the content only (not generated_at), so an unchanged box gets 304.
	content, _ := json.Marshal(body)
	sum := sha256.Sum256(content)
	body.GeneratedAt = s.Now().UTC().Truncate(time.Second)
	s.writeJSON(w, r, `"`+hex.EncodeToString(sum[:16])+`"`, body)
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	entries, first, last, err := logbook.Log{Path: s.LogPath}.Read(after, limit)
	if err != nil {
		s.Log.Error("reading log", "err", err)
		http.Error(w, "can't read the log", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	s.writeJSON(w, r, "", struct {
		Entries []json.RawMessage `json:"entries"`
		First   int64             `json:"first_seq"`
		Last    int64             `json:"last_seq"`
	}{entries, first, last})
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, etag string, v any) {
	if etag != "" {
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(append(data, '\n'))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
