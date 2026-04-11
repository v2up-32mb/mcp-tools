package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type Session struct {
	ID              string
	ProtocolVersion string
	ClientInfo      map[string]any
	InitializedAt   time.Time
	LastSeen        time.Time
}

type Manager struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]Session
}

func NewManager(ttl time.Duration) *Manager {
	return &Manager{ttl: ttl, sessions: make(map[string]Session)}
}

func (m *Manager) Create(protocolVersion string, clientInfo map[string]any) (Session, error) {
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	sess := Session{
		ID:              id,
		ProtocolVersion: protocolVersion,
		ClientInfo:      cloneMap(clientInfo),
		InitializedAt:   now,
		LastSeen:        now,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = sess
	return sess, nil
}

func (m *Manager) Get(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return Session{}, false
	}
	if m.ttl > 0 && time.Since(sess.LastSeen) > m.ttl {
		delete(m.sessions, id)
		return Session{}, false
	}
	sess.LastSeen = time.Now().UTC()
	m.sessions[id] = sess
	return sess, true
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
		now := time.Now()
		for id, sess := range m.sessions {
			if now.Sub(sess.LastSeen) > m.ttl {
				delete(m.sessions, id)
			}
		}
	}
	return len(m.sessions)
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
