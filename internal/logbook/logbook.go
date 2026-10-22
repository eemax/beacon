// Package logbook is beacon's append-only log of runs and events: one JSON object per line,
// each with a seq that increases by one per entry and is never reused (the next seq lives
// in a sidecar file, so pruning old entries can't bring numbers back). Writers on the box
// (collect, exec, event) may run at once: every change holds an exclusive file lock.
package logbook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Entry is a run or an event (spec.Run, spec.Event): validated, with a Seq to set.
type Entry interface {
	Validate() error
	SetSeq(int64)
}

type Log struct{ Path string }

func (l Log) lock() (func(), error) {
	f, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (l Log) nextSeq() (int64, error) {
	data, err := os.ReadFile(l.Path + ".seq")
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s.seq: corrupt (%q)", l.Path, data)
	}
	return n, nil
}

// Append validates the entries, gives each the next seq and appends them. Returns the seqs.
func (l Log) Append(entries ...Entry) ([]int64, error) {
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return nil, err
		}
	}
	unlock, err := l.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	next, err := l.nextSeq()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	seqs := make([]int64, len(entries))
	for i, e := range entries {
		e.SetSeq(next)
		seqs[i] = next
		next++
		line, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	// The sidecar is written after the lines: a crash in between can only skip numbers,
	// never reuse them (the next Append reads the seq from here).
	return seqs, os.WriteFile(l.Path+".seq", []byte(strconv.FormatInt(next, 10)+"\n"), 0o644)
}

type head struct {
	Seq     int64     `json:"seq"`
	Type    string    `json:"type"`
	At      time.Time `json:"at"`
	EndedAt time.Time `json:"ended_at"`
}

func (h head) when() time.Time {
	if h.Type == "run" {
		return h.EndedAt
	}
	return h.At
}

func (l Log) scan(fn func(h head, line []byte) bool) error {
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var h head
		if json.Unmarshal(line, &h) != nil || h.Seq < 1 {
			continue // a torn line from a crash: skip it
		}
		if !fn(h, line) {
			break
		}
	}
	return sc.Err()
}

// Read returns up to limit entries with seq > after, oldest first, as stored, plus the
// first and last seq the log holds now (0, 0 when empty).
func (l Log) Read(after int64, limit int) (entries []json.RawMessage, first, last int64, err error) {
	err = l.scan(func(h head, line []byte) bool {
		if first == 0 {
			first = h.Seq
		}
		last = h.Seq
		if h.Seq > after && len(entries) < limit {
			entries = append(entries, json.RawMessage(append([]byte(nil), line...)))
		}
		return true
	})
	return entries, first, last, err
}

// Prune drops entries older than before, and the oldest beyond keep, rewriting the file
// atomically. Returns how many it dropped.
func (l Log) Prune(before time.Time, keep int) (int, error) {
	unlock, err := l.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	var lines [][]byte
	var whens []time.Time
	if err := l.scan(func(h head, line []byte) bool {
		lines = append(lines, append([]byte(nil), line...))
		whens = append(whens, h.when())
		return true
	}); err != nil {
		return 0, err
	}
	start := 0
	for start < len(lines) && (whens[start].Before(before) || len(lines)-start > keep) {
		start++
	}
	if start == 0 {
		return 0, nil
	}
	var buf bytes.Buffer
	for _, line := range lines[start:] {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := l.Path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return 0, err
	}
	return start, os.Rename(tmp, l.Path)
}
