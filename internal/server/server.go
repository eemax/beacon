// Package server is beacon's read-only HTTP API over a feeds folder (SPEC.md).
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eemax/beacon/internal/feed"
	"github.com/eemax/beacon/internal/tokens"
)

type Server struct {
	Dir     string // feeds folder
	Source  string // this box's name, as the hub knows it
	Version string
	Tokens  *tokens.Store
	Log     *slog.Logger
	Now     func() time.Time
}

type listing struct {
	Schema      int            `json:"schema"`
	Source      string         `json:"source"`
	Beacon      string         `json:"beacon"`
	GeneratedAt time.Time      `json:"generated_at"`
	Feeds       []*feed.Feed   `json:"feeds"`
	Invalid     []feed.Invalid `json:"invalid"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/feeds", s.authed(s.list))
	mux.HandleFunc("GET /v1/feeds/{name}", s.authed(s.one))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
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
		s.Log.Debug("request", "token", name, "path", r.URL.Path)
		next(w, r)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	feeds, invalid, err := feed.LoadAll(s.Dir)
	if err != nil {
		s.Log.Error("reading feeds", "dir", s.Dir, "err", err)
		http.Error(w, "can't read the feeds folder", http.StatusInternalServerError)
		return
	}
	if feeds == nil {
		feeds = []*feed.Feed{}
	}
	if invalid == nil {
		invalid = []feed.Invalid{}
	}
	// The ETag covers the content only, not generated_at, so an unchanged folder gets 304.
	body := listing{Schema: feed.Schema, Source: s.Source, Beacon: s.Version, Feeds: feeds, Invalid: invalid}
	content, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(content)
	body.GeneratedAt = s.Now().UTC().Truncate(time.Second)
	s.writeJSON(w, r, `"`+hex.EncodeToString(sum[:16])+`"`, body)
}

func (s *Server) one(w http.ResponseWriter, r *http.Request) {
	f, ok, err := feed.Load(s.Dir, r.PathValue("name"))
	switch {
	case !ok:
		http.Error(w, "no such feed", http.StatusNotFound)
	case err != nil:
		http.Error(w, "invalid feed: "+err.Error(), http.StatusUnprocessableEntity)
	default:
		content, _ := json.Marshal(f)
		sum := sha256.Sum256(content)
		s.writeJSON(w, r, `"`+hex.EncodeToString(sum[:16])+`"`, f)
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, etag string, v any) {
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(append(data, '\n'))
}
