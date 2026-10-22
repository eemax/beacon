// Package tokens keeps the bearer tokens that may read feeds. The file holds one
// "<name> <sha256-hex>" line per token, never the token itself, so reading the file
// doesn't let anyone in.
package tokens

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const prefix = "bcn_"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type entry struct {
	name string
	hash [sha256.Size]byte
}

// Store is a tokens file, re-read whenever it changes on disk.
type Store struct {
	path string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	entries []entry
	loadErr error
}

func Open(path string) *Store { return &Store{path: path} }

// Check returns the token's name if token is valid. A missing or unreadable file lets
// nobody in.
func (s *Store) Check(token string) (string, bool) {
	entries := s.current()
	sum := sha256.Sum256([]byte(token))
	name, found := "", 0
	for _, e := range entries { // constant time over all entries
		if subtle.ConstantTimeCompare(sum[:], e.hash[:]) == 1 {
			name, found = e.name, 1
		}
	}
	return name, found == 1
}

// Err is the last error loading the file, if any (for logging at startup).
func (s *Store) Err() error {
	s.current()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

func (s *Store) current() []entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	if err != nil {
		s.entries, s.loadErr, s.modTime = nil, err, time.Time{}
		return nil
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size && s.loadErr == nil {
		return s.entries
	}
	entries, err := read(s.path)
	s.entries, s.loadErr, s.modTime, s.size = entries, err, info.ModTime(), info.Size()
	return entries
}

func read(path string) ([]entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []entry
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		raw, err := hex.DecodeString(fields[len(fields)-1])
		if len(fields) != 2 || err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("%s:%d: want \"<name> <sha256-hex>\"", path, line)
		}
		e := entry{name: fields[0]}
		copy(e.hash[:], raw)
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

// Names lists the token names in the file.
func Names(path string) ([]string, error) {
	entries, err := read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	sort.Strings(names)
	return names, err
}

// Add creates a token called name, stores its hash and returns the token. The token is
// only ever shown here.
func Add(path, name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("token name %q: use letters, digits, '.', '_' or '-'", name)
	}
	entries, err := read(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		if e.name == name {
			return "", fmt.Errorf("token %q exists; revoke it first to replace it", name)
		}
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(random)
	e := entry{name: name, hash: sha256.Sum256([]byte(token))}
	return token, write(path, append(entries, e))
}

// Revoke removes the token called name.
func Revoke(path, name string) error {
	entries, err := read(path)
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return fmt.Errorf("no token %q", name)
	}
	return write(path, kept)
}

// write replaces the file atomically, keeping its mode and group if it exists (the
// server's group must stay able to read it).
func write(path string, entries []entry) error {
	var b strings.Builder
	b.WriteString("# beacon tokens: <name> <sha256 of the token>. Manage with `beacon token`.\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s\n", e.name, hex.EncodeToString(e.hash[:]))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tokens.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	mode := os.FileMode(0o640)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		if err := chownLike(tmp, info); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
