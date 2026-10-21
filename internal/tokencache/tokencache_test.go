package tokencache

import (
	"context"
	"fmt"
	"os"
	"sync"
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

// A Client is documented as safe for concurrent use and reaches its Store from
// every in-flight request, so concurrent Get/Put must not race.
func TestMemStoreConcurrentAccess(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("endpoint-%d", i%3)
			for j := 0; j < 50; j++ {
				_ = s.Put(ctx, key, Token{Token: "t"})
				_, _, _ = s.Get(ctx, key)
			}
		}(i)
	}
	wg.Wait()
}

// The zero value must be usable, not panic on first write.
func TestMemStoreZeroValueUsable(t *testing.T) {
	var s MemStore
	if err := s.Put(context.Background(), "k", Token{Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.Get(context.Background(), "k"); !ok || got.Token != "t" {
		t.Errorf("Get = %v, %v; want the stored token", got, ok)
	}
}
