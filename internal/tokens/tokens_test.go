package tokens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddCheckRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	store := Open(path)
	if _, ok := store.Check("anything"); ok {
		t.Fatal("missing file let someone in")
	}

	token, err := Add(path, "lookout")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, prefix) || len(token) < 40 {
		t.Fatalf("weak-looking token %q", token)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), token) || strings.Contains(string(data), token[len(prefix):]) {
		t.Fatal("tokens file contains the token itself")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("new tokens file mode %v, want 0640", info.Mode().Perm())
	}
	if name, ok := store.Check(token); !ok || name != "lookout" {
		t.Fatalf("Check = %q, %v", name, ok)
	}
	if _, ok := store.Check(token + "x"); ok {
		t.Fatal("wrong token accepted")
	}
	if _, err := Add(path, "lookout"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := Add(path, "bad name"); err == nil {
		t.Fatal("bad name accepted")
	}

	second, _ := Add(path, "other")
	if names, _ := Names(path); strings.Join(names, ",") != "lookout,other" {
		t.Fatalf("Names = %v", names)
	}
	if err := Revoke(path, "lookout"); err != nil {
		t.Fatal(err)
	}
	// The store notices the changed file without a restart.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future)
	if _, ok := store.Check(token); ok {
		t.Fatal("revoked token still accepted")
	}
	if _, ok := store.Check(second); !ok {
		t.Fatal("other token lost on revoke")
	}
	if err := Revoke(path, "nobody"); err == nil {
		t.Fatal("revoking a missing token succeeded")
	}
}

func TestMalformedFileLetsNobodyIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	os.WriteFile(path, []byte("lookout not-a-hash\n"), 0o600)
	store := Open(path)
	if _, ok := store.Check("x"); ok || store.Err() == nil {
		t.Fatal("malformed file should refuse everyone and report an error")
	}
}
