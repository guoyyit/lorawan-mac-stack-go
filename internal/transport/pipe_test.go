package transport

import (
	"context"
	"testing"
	"time"
)

func TestPipeDeliversBothWays(t *testing.T) {
	p := NewPipe()
	gotA := make(chan []byte, 1)
	gotB := make(chan []byte, 1)
	p.DeviceEnd().SetHandler(func(b []byte) { gotA <- b })
	p.ServerEnd().SetHandler(func(b []byte) { gotB <- b })

	ctx := context.Background()
	if err := p.DeviceEnd().Send(ctx, []byte("up")); err != nil {
		t.Fatal(err)
	}
	if err := p.ServerEnd().Send(ctx, []byte("down")); err != nil {
		t.Fatal(err)
	}
	if string(<-gotB) != "up" {
		t.Fatal("server did not get uplink")
	}
	if string(<-gotA) != "down" {
		t.Fatal("device did not get downlink")
	}
	s, r, d := p.DeviceEnd().Stats()
	if s != 1 || r != 1 || d != 0 {
		t.Fatalf("device stats = %d %d %d", s, r, d)
	}
}

func TestPipeDropNext(t *testing.T) {
	p := NewPipe()
	got := make(chan []byte, 1)
	p.ServerEnd().SetHandler(func(b []byte) { got <- b })
	p.DeviceEnd().DropNext(2)
	ctx := context.Background()
	_ = p.DeviceEnd().Send(ctx, []byte("lost1"))
	_ = p.DeviceEnd().Send(ctx, []byte("lost2"))
	_ = p.DeviceEnd().Send(ctx, []byte("kept"))
	select {
	case m := <-got:
		if string(m) != "kept" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("frame after drop budget never arrived")
	}
	if _, _, d := p.DeviceEnd().Stats(); d != 2 {
		t.Fatalf("dropped = %d", d)
	}
}

func TestPipeClose(t *testing.T) {
	p := NewPipe()
	p.DeviceEnd().Close()
	if err := p.DeviceEnd().Send(context.Background(), []byte("x")); err != ErrClosed {
		t.Fatalf("send after close = %v", err)
	}
}
