package logbook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/spec"
)

func event(at time.Time) *spec.Event {
	return &spec.Event{Type: "event", ID: at.Format("150405.000000000"), At: at, Kind: "note", Title: "n"}
}

func TestAppendReadAndSeqNeverReused(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "log.jsonl")}
	now := time.Now().UTC()
	seqs, err := l.Append(event(now.Add(-100*24*time.Hour)), event(now.Add(-time.Hour)), event(now))
	if err != nil || len(seqs) != 3 || seqs[0] != 1 || seqs[2] != 3 {
		t.Fatalf("seqs=%v err=%v", seqs, err)
	}
	entries, first, last, _ := l.Read(1, 10)
	if len(entries) != 2 || first != 1 || last != 3 {
		t.Fatalf("read after 1: %d entries, %d-%d", len(entries), first, last)
	}
	if n, _ := l.Prune(now.AddDate(0, 0, -90), 1000); n != 1 {
		t.Fatalf("pruned %d, want the 100-day-old one", n)
	}
	if n, _ := l.Prune(time.Time{}, 1); n != 1 {
		t.Fatalf("pruned %d, want 1 over the cap", n)
	}
	l.Append(event(now))
	_, first, last, _ = l.Read(0, 10)
	if first != 3 || last != 4 {
		t.Fatalf("after pruning: %d-%d, want 3-4 (seq keeps counting)", first, last)
	}
	if _, err := l.Append(&spec.Event{Type: "event"}); err == nil {
		t.Fatal("invalid entry appended")
	}
}

func TestConcurrentAppendsAndTornLines(t *testing.T) {
	l := Log{Path: filepath.Join(t.TempDir(), "log.jsonl")}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Append(event(time.Now().Add(time.Duration(i) * time.Microsecond)))
		}(i)
	}
	wg.Wait()
	f, _ := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte(`{"seq":21,"type":"ev`)) // a crash mid-line
	f.Close()
	entries, _, last, err := l.Read(0, 100)
	if err != nil || len(entries) != 20 || last != 20 {
		t.Fatalf("%d entries, last %d, %v", len(entries), last, err)
	}
	seen := map[int64]bool{}
	for _, e := range entries {
		var h struct{ Seq int64 }
		json.Unmarshal(e, &h)
		if seen[h.Seq] {
			t.Fatalf("seq %d twice", h.Seq)
		}
		seen[h.Seq] = true
	}
}
