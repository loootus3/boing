package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// flakyStore fails every save while failing is set and records the last saved token.
type flakyStore struct {
	mu      sync.Mutex
	failing bool
	saves   int
	last    string
}

func (s *flakyStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *flakyStore) Delete(context.Context, string) error  { return nil }
func (s *flakyStore) Save(_ context.Context, a *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.failing {
		return "", errors.New("no space left on device")
	}
	s.last, _ = a.Metadata["refresh_token"].(string)
	return "", nil
}

func (s *flakyStore) state() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves, s.last
}

func TestFailedSaveIsRetried(t *testing.T) {
	store := &flakyStore{}
	m := NewManager(store, nil, nil)
	m.persistRetryMin, m.persistRetryMax = 10*time.Millisecond, 20*time.Millisecond
	ctx := context.Background()
	if _, err := m.Register(ctx, &Auth{ID: "acct.json", Provider: "claude", Metadata: map[string]any{"refresh_token": "old"}}); err != nil {
		t.Fatal(err)
	}
	if _, last := store.state(); last != "old" {
		t.Fatalf("initial save: got %q", last)
	}

	// A refresh rotates the token while the disk is full.
	store.mu.Lock()
	store.failing = true
	store.mu.Unlock()
	cur, _ := m.GetByID("acct.json")
	cur = cur.Clone()
	cur.Metadata = map[string]any{"refresh_token": "rotated"}
	if _, err := m.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, pending := m.persistPending.Load("acct.json"); !pending {
		t.Fatal("failed save not marked pending")
	}
	time.Sleep(50 * time.Millisecond)
	if saves, last := store.state(); saves < 3 || last != "old" {
		t.Fatalf("expected retries while the disk is full: saves=%d last=%q", saves, last)
	}

	// Space comes back: the retry writes the rotated token and stops.
	store.mu.Lock()
	store.failing = false
	store.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, last := store.state(); last == "rotated" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotated token never saved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for time.Now().Before(deadline) {
		if _, running := m.persistRetrying.Load("acct.json"); !running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, pending := m.persistPending.Load("acct.json"); pending {
		t.Fatal("still pending after a successful save")
	}
	if _, running := m.persistRetrying.Load("acct.json"); running {
		t.Fatal("retry loop still running after a successful save")
	}
}
