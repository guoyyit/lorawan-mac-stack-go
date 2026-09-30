package lorawan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMemoryStoreCRUD(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	s := NewSession(EUI{1}, EUI{2}, AESKey{3})
	s.FCntUp = 11
	id := EUIHex(EUI{1})
	if err := st.Save(ctx, id, s); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(ctx, id)
	if err != nil || got.FCntUp != 11 {
		t.Fatalf("load = %+v, %v", got, err)
	}
	got.FCntUp = 99
	again, _ := st.Load(ctx, id)
	if again.FCntUp != 11 {
		t.Fatal("Load returned mutable reference")
	}
	if err := st.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestFileStorePersistence(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	id := EUIHex(EUI{0xde, 0xad})
	s := NewSession(EUI{0xde, 0xad}, EUI{2}, AESKey{4})
	s.Joined = true
	s.FCntDown = 7
	s.NwkSKey = AESKey{1, 2, 3}

	st1, err := NewFileStore(FileStoreConfig{Dir: dir, Fsync: true, AtomicRename: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.Save(ctx, id, s); err != nil {
		t.Fatal(err)
	}

	// Reopen: simulates process restart.
	st2, err := NewFileStore(FileStoreConfig{Dir: dir, AtomicRename: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st2.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Joined || got.FCntDown != 7 || got.NwkSKey != (AESKey{1, 2, 3}) {
		t.Fatalf("restored = %+v", got)
	}
}

func TestFileStoreCorruptRecordIsolated(t *testing.T) {
	dir := t.TempDir()
	id := "abcdef"
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(FileStoreConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(context.Background(), id); err == nil {
		t.Fatal("expected error for corrupt record")
	}
	// Other records remain usable.
	if err := st.Save(context.Background(), "111111", NewSession(EUI{}, EUI{}, AESKey{})); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreConcurrentSaves(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(FileStoreConfig{Dir: dir, AtomicRename: true, Fsync: false})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			id := EUIHex(EUI{byte(i)})
			for j := 0; j < 10; j++ {
				s := NewSession(EUI{byte(i)}, EUI{}, AESKey{})
				s.FCntUp = uint32(j)
				if err := st.Save(context.Background(), id, s); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	list, err := st.List(context.Background())
	if err != nil || len(list) != 20 {
		t.Fatalf("list=%v err=%v", list, err)
	}
}
