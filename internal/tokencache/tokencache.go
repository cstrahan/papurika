// Package tokencache persists salt-api session tokens between invocations,
// keyed by endpoint so different profiles/masters never collide (fixes pepper's
// single shared cache, upstream pepper#205). Files are written 0600 under a 0700
// directory.
package tokencache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Token is a cached session token.
type Token struct {
	Token  string    `json:"token"`
	Expire time.Time `json:"expire"`
	Perms  []string  `json:"perms,omitempty"`
}

// Valid reports whether the token is non-empty and not past its expiry (with a
// small skew so we re-auth before the server would reject it).
func (t Token) Valid(skew time.Duration) bool {
	return t.Token != "" && time.Now().Add(skew).Before(t.Expire)
}

// Key derives a stable cache key from the endpoint identity. Two contexts that
// differ in master URL, user, or eauth get distinct keys.
func Key(masterURL, user, eauth string) string {
	sum := sha256.Sum256([]byte(masterURL + "\x00" + user + "\x00" + eauth))
	return hex.EncodeToString(sum[:])[:16]
}

// Store persists tokens by key.
type Store interface {
	Get(ctx context.Context, key string) (Token, bool, error)
	Put(ctx context.Context, key string, t Token) error
}

// FileStore is a filesystem-backed Store: one JSON file per key under dir.
type FileStore struct {
	dir string
}

// NewFileStore returns a FileStore rooted at dir (created lazily, 0700, on Put).
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

func (s *FileStore) path(key string) string { return filepath.Join(s.dir, key+".json") }

func (s *FileStore) Get(_ context.Context, key string) (Token, bool, error) {
	b, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		// A corrupt cache file is not fatal — treat as a miss.
		return Token{}, false, nil
	}
	return t, true, nil
}

func (s *FileStore) Put(_ context.Context, key string, t Token) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	// Write atomically with 0600 perms.
	tmp, err := os.CreateTemp(s.dir, ".tok-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(key))
}

// MemStore is an in-memory Store (useful for tests and ephemeral clients).
//
// Guarded by a mutex because a Client is documented as safe for concurrent use
// and reaches its Store from every in-flight request: concurrent Run calls will
// Get and Put here at the same time.
type MemStore struct {
	mu sync.RWMutex
	m  map[string]Token
}

func NewMemStore() *MemStore { return &MemStore{m: map[string]Token{}} }

func (s *MemStore) Get(_ context.Context, key string) (Token, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.m[key]
	return t, ok, nil
}

func (s *MemStore) Put(_ context.Context, key string, t Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Token{}
	}
	s.m[key] = t
	return nil
}
