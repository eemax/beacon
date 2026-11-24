package spec_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/eemax/beacon/internal/collect"
	"github.com/eemax/beacon/internal/spec"
)

// section is one document kind of the corpus.
type section struct {
	Valid   []json.RawMessage `json:"valid"`
	Invalid []struct {
		Why string          `json:"why"`
		Doc json.RawMessage `json:"doc"`
	} `json:"invalid"`
}

// loadCorpus reads testdata/contract/beacon-v3.json: the contract shared with every reader
// of schema 3, read only (a disagreement is a finding against it, never an edit).
func loadCorpus(t *testing.T) map[string]section {
	t.Helper()
	data, err := os.ReadFile("../../testdata/contract/beacon-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	delete(all, "schema")
	out := map[string]section{}
	for k, v := range all {
		var sec section
		if err := json.Unmarshal(v, &sec); err != nil {
			t.Fatal(err)
		}
		out[k] = sec
	}
	return out
}

func TestContract(t *testing.T) {
	validators := map[string]func([]byte) error{
		"envelope":   func(b []byte) error { _, err := spec.ParseEnvelope(b); return err },
		"served":     func(b []byte) error { _, err := spec.ParseComponent(b); return err },
		"component":  func(b []byte) error { _, err := spec.ParseProducerComponent(b); return err },
		"adapter":    func(b []byte) error { _, err := spec.ParseAdapter(b); return err },
		"run":        func(b []byte) error { _, err := spec.ParseRun(b); return err },
		"definition": func(b []byte) error { _, err := collect.ParseDefinition(b); return err },
	}
	sections := loadCorpus(t)
	if len(sections) != len(validators) {
		t.Fatalf("the corpus has %d sections, beacon %d validators", len(sections), len(validators))
	}
	for name, validate := range validators {
		sec, ok := sections[name]
		if !ok {
			t.Fatalf("no %q section in the corpus", name)
		}
		t.Run(name, func(t *testing.T) {
			if len(sec.Valid) == 0 || len(sec.Invalid) == 0 {
				t.Fatal("empty section")
			}
			for i, doc := range sec.Valid {
				if err := validate(doc); err != nil {
					t.Errorf("valid case %d refused: %v", i, err)
				}
			}
			for _, c := range sec.Invalid {
				if err := validate(c.Doc); err == nil {
					t.Errorf("invalid case accepted: %s", c.Why)
				}
			}
		})
	}

	// The same component with reporting: served, never produced.
	t.Run("reporting", func(t *testing.T) {
		var served []json.RawMessage
		for _, d := range sections["served"].Valid {
			var c spec.Component
			if json.Unmarshal(d, &c) == nil && c.Reporting != nil {
				served = append(served, d)
			}
		}
		if len(served) == 0 {
			t.Fatal("no served case carries reporting")
		}
		for _, d := range served {
			if _, err := spec.ParseComponent(d); err != nil {
				t.Errorf("--served refused it: %v", err)
			}
			if _, err := spec.ParseProducerComponent(d); err == nil {
				t.Error("--component accepted reporting")
			}
		}
	})
}
