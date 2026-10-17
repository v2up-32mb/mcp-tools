package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

type Session struct {
	ID              string
	ProtocolVersion string
	ClientInfo      map[string]any
	InitializedAt   time.Time
	LastSeen        time.Time
}

type sessionEntry struct {
	ID              string
	ProtocolVersion string
	ClientInfo      map[string]any
	InitializedAt   time.Time
	lastSeenUnix    atomic.Int64 // Unix timestamp
}

type Manager struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]*sessionEntry
}

func NewManager(ttl time.Duration) *Manager {
	return &Manager{ttl: ttl, sessions: make(map[string]*sessionEntry)}
}

func (m *Manager) Create(protocolVersion string, clientInfo map[string]any) (Session, error) {
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	entry := &sessionEntry{
		ID:              id,
		ProtocolVersion: protocolVersion,
		ClientInfo:      cloneMap(clientInfo),
		InitializedAt:   now,
	}
	entry.lastSeenUnix.Store(now.Unix())
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = entry
	return Session{
		ID:              entry.ID,
		ProtocolVersion: entry.ProtocolVersion,
		ClientInfo:      entry.ClientInfo,
		InitializedAt:   entry.InitializedAt,
		LastSeen:        now,
	}, nil
}

func (m *Manager) Get(id string) (Session, bool) {
	m.mu.RLock()
	entry, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return Session{}, false
	}
	lastSeenUnix := entry.lastSeenUnix.Load()
	lastSeen := time.Unix(lastSeenUnix, 0).UTC()
	if m.ttl > 0 && time.Since(lastSeen) > m.ttl {
		m.mu.Lock()
		// Re-check after acquiring write lock
		if e, stillExists := m.sessions[id]; stillExists {
			lastSeenUnix := e.lastSeenUnix.Load()
			lastSeen := time.Unix(lastSeenUnix, 0).UTC()
			if time.Since(lastSeen) > m.ttl {
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
		return Session{}, false
	}
	now := time.Now().UTC()
	entry.lastSeenUnix.Store(now.Unix())
	return Session{
		ID:              entry.ID,
		ProtocolVersion: entry.ProtocolVersion,
		ClientInfo:      entry.ClientInfo,
		InitializedAt:   entry.InitializedAt,
		LastSeen:        now,
	}, true
}

func (m *Manager) PruneExpired() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pruneExpiredLocked(time.Now())
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ttl > 0 {
		m.pruneExpiredLocked(time.Now())
	}
	return len(m.sessions)
}

func (m *Manager) pruneExpiredLocked(now time.Time) []string {
	if m.ttl <= 0 {
		return nil
	}
	var expired []string
	for id, entry := range m.sessions {
		lastSeenUnix := entry.lastSeenUnix.Load()
		lastSeen := time.Unix(lastSeenUnix, 0).UTC()
		if now.Sub(lastSeen) > m.ttl {
			delete(m.sessions, id)
			expired = append(expired, id)
		}
	}
	return expired
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
