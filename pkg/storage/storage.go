// Package storage provides crash-safe persistence of protocol state:
// session keys, device address and frame counters survive process restarts
// through atomic write-replace JSON files.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound is returned by Load when no document exists for a key.
var ErrNotFound = errors.New("storage: key not found")

// keyValues is the on-disk envelope. Keeping the original key next to its
// JSON value lets Keys list keys without decoding the values and rejects
// filename collisions (different keys hashing to the same file).
type keyValues struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// Store is a persistent key-scoped JSON document store. Each Save is
// crash-atomic: bytes go to a temp file, are fsynced and renamed over the
// destination, so a crash leaves either the previous document or the new
// one, never a torn write.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open opens (creating if needed) a file-backed store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("storage: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// MustOpen is Open that panics on error (tests/demo wiring).
func MustOpen(dir string) *Store {
	s, err := Open(dir)
	if err != nil {
		panic(err)
	}
	return s
}

func (s *Store) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}

// Save atomically writes v as JSON under key.
func (s *Store) Save(key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(keyValues{Key: key, Value: data})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	final := s.path(key)
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(envelope); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	cleanup = false

	// Best-effort directory fsync so the rename itself is durable.
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Load reads the JSON document stored under key into v.
func (s *Store) Load(key string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var env keyValues
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("storage: corrupt document for %q: %w", key, err)
	}
	if env.Key != key {
		return fmt.Errorf("storage: key envelope mismatch (have %q want %q)", env.Key, key)
	}
	return json.Unmarshal(env.Value, v)
}

// Delete removes the document stored under key.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Keys lists every stored key in sorted order.
func (s *Store) Keys() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return nil, err
		}
		var env keyValues
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // ignore torn/foreign files
		}
		keys = append(keys, env.Key)
	}
	sort.Strings(keys)
	return keys, nil
}

// MemoryStore is an in-memory Store used in tests that do not exercise
// persistence.
type MemoryStore struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]json.RawMessage)}
}

// Save writes v as JSON under key.
func (s *MemoryStore) Save(key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = data
	return nil
}

// Load reads the JSON document stored under key into v.
func (s *MemoryStore) Load(key string, v any) error {
	s.mu.Lock()
	data, ok := s.data[key]
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	return json.Unmarshal(data, v)
}

// Delete removes the document stored under key.
func (s *MemoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

// Keys lists every stored key in sorted order.
func (s *MemoryStore) Keys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}
