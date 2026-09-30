package lorawan

import (
	"context"
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

// EUIHex returns the lowercase hex identifier used as a storage key.
func EUIHex(id EUI) string {
	return hex.EncodeToString(id[:])
}

// Store persists session records by string identifier (DevEUI hex).
type Store interface {
	Load(ctx context.Context, id string) (*Session, error)
	Save(ctx context.Context, id string, s *Session) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]string, error)
}

// NewMemoryStore returns a non-persistent, concurrency-safe store.
func NewMemoryStore() Store { return &memoryStore{} }

type memoryStore struct {
	mu sync.RWMutex
	m  map[string]*Session
}

func (s *memoryStore) Load(_ context.Context, id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return v.Clone(), nil
}

func (s *memoryStore) Save(_ context.Context, id string, sess *Session) error {
	if sess == nil {
		return ErrMalformed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]*Session)
	}
	s.m[id] = sess.Clone()
	return nil
}

func (s *memoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return ErrNotFound
	}
	delete(s.m, id)
	return nil
}

func (s *memoryStore) List(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.m))
	for id := range s.m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// FileStoreConfig configures JSON file persistence.
type FileStoreConfig struct {
	Dir          string
	Fsync        bool
	AtomicRename bool
}

// NewFileStore opens (creating if needed) a JSON-file backed store.
func NewFileStore(cfg FileStoreConfig) (Store, error) {
	if cfg.Dir == "" {
		return nil, errors.New("lorawan: empty store dir")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("lorawan: create store dir: %w", err)
	}
	return &fileStore{cfg: cfg, cache: make(map[string]*Session)}, nil
}

type fileStore struct {
	cfg   FileStoreConfig
	mu    sync.Mutex
	cache map[string]*Session
}

func (s *fileStore) path(id string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'f' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, id)
	return filepath.Join(s.cfg.Dir, name+".json")
}

func (s *fileStore) Load(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.cache[id]; ok {
		return sess.Clone(), nil
	}
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		// Corrupt on-disk record: surface an error, never cache partial state.
		return nil, fmt.Errorf("lorawan: corrupt session %s: %w", id, err)
	}
	s.cache[id] = &sess
	return sess.Clone(), nil
}

func (s *fileStore) Save(_ context.Context, id string, sess *Session) error {
	if sess == nil {
		return ErrMalformed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	final := s.path(id)
	if !s.cfg.AtomicRename {
		if err := os.WriteFile(final, raw, 0o600); err != nil {
			return err
		}
		if s.cfg.Fsync {
			return fsyncFile(final)
		}
		s.cache[id] = sess.Clone()
		return nil
	}
	tmp, err := os.CreateTemp(s.cfg.Dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if s.cfg.Fsync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	s.cache[id] = sess.Clone()
	return nil
}

func fsyncFile(name string) error {
	f, err := os.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *fileStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, id)
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (s *fileStore) List(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out, nil
}
