package mac

// serverMeta holds non-per-device persistent server state: the monotonic
// counter used to allocate deterministic AppNonce/DevAddr values so they
// never repeat after a server restart.
type serverMeta struct {
	NonceCounter uint32 `json:"nonce_counter"`
	AddrCounter  uint32 `json:"addr_counter"`
}

const serverMetaKey = "server-meta"

// loadMetaLocked must be called with s.metaMu held.
func (s *NetworkServer) loadMetaLocked() serverMeta {
	var m serverMeta
	if err := s.cfg.Store.Load(serverMetaKey, &m); err != nil && !IsNotFound(err) {
		s.log("warn", "cannot load server meta: "+err.Error())
	}
	return m
}

// nextIdentifiers allocates a fresh unique AppNonce/DevAddr pair. The
// persisted monotonic counters guarantee no reuse after a restart.
func (s *NetworkServer) nextIdentifiers() (appNonce [3]byte, devAddr [4]byte) {
	if s.cfg.AppNonce != nil {
		// Test-injected nonce: DevAddr still comes from the persisted counter.
		s.metaMu.Lock()
		m := s.loadMetaLocked()
		m.AddrCounter++
		_ = s.cfg.Store.Save(serverMetaKey, &m)
		s.metaMu.Unlock()
		var addr [4]byte
		addr[0], addr[1] = 0x2e, 0x1a
		addr[2] = byte(m.AddrCounter >> 8)
		addr[3] = byte(m.AddrCounter)
		return s.cfg.AppNonce(), addr
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	m := s.loadMetaLocked()
	m.NonceCounter++
	m.AddrCounter++
	if err := s.cfg.Store.Save(serverMetaKey, &m); err != nil {
		s.log("warn", "cannot persist server meta: "+err.Error())
	}

	var nonce [3]byte
	nonce[0] = byte(m.NonceCounter)
	nonce[1] = byte(m.NonceCounter >> 8)
	nonce[2] = byte(m.NonceCounter >> 16)

	var addr [4]byte
	addr[0] = 0x2e // DevAddr prefix (LSB-first on the wire later)
	addr[1] = 0x1a
	addr[2] = byte(m.AddrCounter >> 8)
	addr[3] = byte(m.AddrCounter)
	return nonce, addr
}
