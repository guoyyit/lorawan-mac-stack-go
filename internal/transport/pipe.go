// Package transport provides an in-memory loopback "air" between devices
// and the network server.
package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// FrameHandler receives a raw frame delivered to an endpoint.
type FrameHandler func(raw []byte)

// Medium is the shared in-memory "air". A server endpoint receives frames
// from every device endpoint; every device endpoint receives frames sent by
// the server endpoint. This models one network server serving many devices.
type Medium struct {
	mu      sync.RWMutex
	server  *Endpoint
	devices map[*Endpoint]struct{}
}

// Endpoint is one attachment point on a Medium.
type Endpoint struct {
	mu       sync.Mutex
	medium   *Medium
	isServer bool
	handler  FrameHandler
	latency  time.Duration
	dropNext int
	closed   bool
	sent     int64
	received int64
	dropped  int64
}

// NewMedium creates an empty air.
func NewMedium() *Medium {
	return &Medium{devices: make(map[*Endpoint]struct{})}
}

// NewPipe creates a medium with one server and one device endpoint attached.
// It is a convenience for single-device use and existing call sites.
func NewPipe() *Medium {
	m := NewMedium()
	m.ServerEnd()
	m.DeviceEnd()
	return m
}

// ServerEnd creates (once) and returns the server endpoint.
func (m *Medium) ServerEnd() *Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server != nil {
		return m.server
	}
	e := &Endpoint{medium: m, isServer: true}
	m.server = e
	return e
}

// DeviceEnd returns the first device endpoint, or creates one if none exists.
func (m *Medium) DeviceEnd() *Endpoint {
	m.mu.RLock()
	for e := range m.devices {
		m.mu.RUnlock()
		return e
	}
	m.mu.RUnlock()
	return m.NewDeviceEnd()
}

// NewDeviceEnd attaches a new device endpoint and returns it.
func (m *Medium) NewDeviceEnd() *Endpoint {
	e := &Endpoint{medium: m}
	m.mu.Lock()
	m.devices[e] = struct{}{}
	m.mu.Unlock()
	return e
}

// Send transmits a frame. Server->device frames fan out to all device
// endpoints; device->server frames go only to the server endpoint. Per-device
// loss/latency is applied at the sending endpoint.
func (e *Endpoint) Send(ctx context.Context, raw []byte) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	latency := e.latency
	if e.dropNext > 0 {
		e.dropNext--
		atomic.AddInt64(&e.dropped, 1)
		e.mu.Unlock()
		return nil
	}
	atomic.AddInt64(&e.sent, 1)
	m := e.medium
	fromServer := e.isServer
	e.mu.Unlock()

	payload := append([]byte(nil), raw...)
	recipients := m.recipients(fromServer)
	for _, r := range recipients {
		r.schedule(ctx, payload, latency)
	}
	return nil
}

func (m *Medium) recipients(fromServer bool) []*Endpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if fromServer {
		out := make([]*Endpoint, 0, len(m.devices))
		for e := range m.devices {
			out = append(out, e)
		}
		return out
	}
	if m.server == nil {
		return nil
	}
	return []*Endpoint{m.server}
}

func (e *Endpoint) schedule(ctx context.Context, payload []byte, latency time.Duration) {
	if latency <= 0 {
		e.deliver(payload)
		return
	}
	go func() {
		t := time.NewTimer(latency)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
			e.deliver(payload)
		}
	}()
}

func (e *Endpoint) deliver(raw []byte) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	h := e.handler
	atomic.AddInt64(&e.received, 1)
	e.mu.Unlock()
	if h != nil {
		h(raw)
	}
}

// SetHandler installs the callback for frames arriving at this endpoint.
func (e *Endpoint) SetHandler(h FrameHandler) {
	e.mu.Lock()
	e.handler = h
	e.mu.Unlock()
}

// SetLatency adds a fixed one-way propagation delay to frames SENT from here.
func (e *Endpoint) SetLatency(d time.Duration) {
	e.mu.Lock()
	e.latency = d
	e.mu.Unlock()
}

// DropNext arranges for the next n frames SENT from this endpoint to be lost.
func (e *Endpoint) DropNext(n int) {
	e.mu.Lock()
	if n > 0 {
		e.dropNext += n
	}
	e.mu.Unlock()
}

// Stats reports sent/received/dropped counters for observability.
func (e *Endpoint) Stats() (sent, received, dropped int64) {
	return atomic.LoadInt64(&e.sent), atomic.LoadInt64(&e.received), atomic.LoadInt64(&e.dropped)
}

// Close shuts the endpoint; future Sends fail and delayed frames arriving
// afterwards are discarded.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return nil
}
