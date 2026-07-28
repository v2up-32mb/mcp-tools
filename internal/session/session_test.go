package session

import (
	"testing"
	"time"
)

func TestManagerPruneExpiredReturnsRemovedSessionIDs(t *testing.T) {
	manager := NewManager(time.Minute)
	sess, err := manager.Create("2025-11-25", nil)
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}

	manager.mu.Lock()
	expired := manager.sessions[sess.ID]
	expired.LastSeen = time.Now().Add(-2 * time.Minute)
	manager.sessions[sess.ID] = expired
	manager.mu.Unlock()

	removed := manager.PruneExpired()
	if len(removed) != 1 || removed[0] != sess.ID {
		t.Fatalf("unexpected expired IDs: %#v", removed)
	}
	if _, ok := manager.Get(sess.ID); ok {
		t.Fatal("expected expired session to be removed")
	}
}
