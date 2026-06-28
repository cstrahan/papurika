package tokencache

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestKeyIsolatesEndpoints(t *testing.T) {
	a := Key("https://m1:8000", "alice", "pam")
	b := Key("https://m2:8000", "alice", "pam")
	c := Key("https://m1:8000", "bob", "pam")
	d := Key("https://m1:8000", "alice", "file")
	if a == b || a == c || a == d {
		t.Errorf("keys should differ by endpoint/user/eauth: %s %s %s %s", a, b, c, d)
	}
	if a != Key("https://m1:8000", "alice", "pam") {
		t.Errorf("key should be stable")
	}
}

func TestFileStoreRoundTripAndPerms(t *testing.T) {
	dir := t.TempDir() + "/tokens"
	s := NewFileStore(dir)
	ctx := context.Background()

	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("expected miss on empty store")
	}

	tok := Token{Token: "abc", Expire: time.Now().Add(time.Hour), Perms: []string{".*"}}
	if err := s.Put(ctx, "k", tok); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("get after put: ok=%v err=%v", ok, err)
	}
	if got.Token != "abc" || len(got.Perms) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// File must be 0600.
	fi, err := os.Stat(s.path("k"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestTokenValid(t *testing.T) {
	if (Token{Token: "x", Expire: time.Now().Add(time.Hour)}).Valid(time.Minute) != true {
		t.Error("fresh token should be valid")
	}
	if (Token{Token: "x", Expire: time.Now().Add(10 * time.Second)}).Valid(time.Minute) {
		t.Error("token within skew window should be invalid")
	}
	if (Token{Token: "", Expire: time.Now().Add(time.Hour)}).Valid(time.Minute) {
		t.Error("empty token should be invalid")
	}
}
