package subsusage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	var prev time.Duration
	for i, w := range want {
		prev = nextBackoff(prev)
		if prev != w {
			t.Fatalf("step %d = %v, want %v", i, prev, w)
		}
	}
}

func TestGetReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	tr := &Tracker{client: srv.Client()}
	_, err := tr.get(context.Background(), srv.URL, nil)
	if err == nil || err.Error() != "HTTP 429" {
		t.Fatalf("err = %v", err)
	}
	if !isRateLimited(err) || !isRateLimited(fmt.Errorf("wrap: %w", err)) {
		t.Fatal("429 not detected")
	}
	if isRateLimited(errors.New("HTTP 429")) || isRateLimited(&httpStatusError{Code: 401}) {
		t.Fatal("false positive")
	}
}

func TestCarryEntry(t *testing.T) {
	now := time.Now()
	prev := &Entry{AuthID: "a", Plan: "Max", Error: "HTTP 429", DataAt: now.Add(-10 * time.Minute),
		Windows: []Window{{Kind: "weekly", UsedPercent: 10}}, LimitReached: true}
	until := now.Add(time.Minute)
	got := carryEntry(prev, until, now)
	if len(got.Windows) != 1 || got.Plan != "Max" || got.Error != "HTTP 429" || !got.DataAt.Equal(prev.DataAt) ||
		!got.LimitReached || got.RetryAt == nil || !got.RetryAt.Equal(until) {
		t.Fatalf("carried = %+v", got)
	}
	got.Windows[0].UsedPercent = 99
	if prev.Windows[0].UsedPercent != 10 || prev.RetryAt != nil {
		t.Fatal("previous entry was mutated")
	}
	prev.DataAt = now.Add(-staleAfter - time.Minute)
	if len(carryEntry(prev, until, now).Windows) != 0 {
		t.Fatal("stale windows kept")
	}
}
