package storage

import (
	"errors"
	"testing"
)

type doc struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := MustOpen(t.TempDir())

	in := doc{Name: "session", Value: 42}
	if err := s.Save("k1", &in); err != nil {
		t.Fatal(err)
	}
	var out doc
	if err := s.Load("k1", &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip %+v", out)
	}

	// overwrite keeps the same key
	in.Value = 43
	if err := s.Save("k1", &in); err != nil {
		t.Fatal(err)
	}
	if err := s.Load("k1", &out); err != nil || out.Value != 43 {
		t.Fatalf("overwrite: %+v err %v", out, err)
	}

	keys, err := s.Keys()
	if err != nil || len(keys) != 1 || keys[0] != "k1" {
		t.Fatalf("keys=%v err=%v", keys, err)
	}

	if err := s.Delete("k1"); err != nil {
		t.Fatal(err)
	}
	var missing doc
	if err := s.Load("k1", &missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Save("a", doc{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := s.Load("a", &d); err != nil || d.Name != "x" {
		t.Fatalf("load %+v err %v", d, err)
	}
	var miss doc
	if err := s.Load("nope", &miss); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
