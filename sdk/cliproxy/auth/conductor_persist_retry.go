package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Retry schedule for a credential whose save failed: start fast, then settle at the maximum,
// giving up after about a day of failures.
const (
	persistRetryMinDelay = 30 * time.Second
	persistRetryMaxDelay = 5 * time.Minute
	persistRetryAttempts = 300
)

// persistRetryDelays returns the retry schedule; tests shorten it per manager.
func (m *Manager) persistRetryDelays() (time.Duration, time.Duration) {
	if m.persistRetryMin > 0 && m.persistRetryMax > 0 {
		return m.persistRetryMin, m.persistRetryMax
	}
	return persistRetryMinDelay, persistRetryMaxDelay
}

// notePersistResult tracks failed saves and retries them in the background.
//
// A refresh rotates the OAuth refresh token and keeps the new one only in memory when the save
// fails (for example on a full disk). Without a later save, the next restart loads the old,
// already rotated token from disk and the account has to sign in again. It must not block:
// persistLocked calls it with m.mu released.
//
// Cancelled saves (a client gone, a preempted save) are not retried; a later write covers them.
// Retries are timers rather than a sleeping goroutine, so nothing stays blocked between
// attempts.
func (m *Manager) notePersistResult(id string, err error) {
	if m == nil || id == "" {
		return
	}
	if err == nil {
		m.persistPending.Delete(id)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	m.persistPending.Store(id, struct{}{})
	if _, running := m.persistRetrying.LoadOrStore(id, struct{}{}); running {
		return
	}
	first, _ := m.persistRetryDelays()
	m.schedulePersistRetry(id, first, 1)
}

func (m *Manager) schedulePersistRetry(id string, delay time.Duration, attempt int) {
	time.AfterFunc(delay, func() { m.retryPersist(id, delay, attempt) })
}

// retryPersist re-saves the current in-memory state of an auth. The save runs through Modify,
// so it takes the same per-auth mutation gate and out-of-order guard as every other write and
// always writes the latest state. It reschedules itself until a save succeeds, the auth or its
// file is gone, or it runs out of attempts.
func (m *Manager) retryPersist(id string, delay time.Duration, attempt int) {
	if _, pending := m.persistPending.Load(id); pending {
		if m.persistFileRemoved(id) {
			// Removed on purpose (e.g. deleted through the management API): do not recreate it.
			m.persistPending.Delete(id)
		} else {
			// A file snapshot ties a replace to the file it was read from; after a failed save
			// the disk may not match, which would make the replace a no-op instead of a save.
			_, found, errModify := m.Modify(context.Background(), id, 0, func(a *Auth) error {
				a.ClearFileSnapshot()
				return nil
			})
			_, still := m.persistPending.Load(id)
			switch {
			case errModify == nil && !found:
				m.persistPending.Delete(id)
			case still && attempt < persistRetryAttempts:
				_, maxDelay := m.persistRetryDelays()
				m.schedulePersistRetry(id, min(delay*2, maxDelay), attempt+1)
				return
			case still:
				m.persistPending.Delete(id)
				log.WithField("auth_id", id).Errorf("giving up saving auth after %d failed attempts; it may need a new login after a restart", attempt)
			default:
				log.WithField("auth_id", id).Info("saved auth after an earlier failed save")
			}
		}
	}
	m.persistRetrying.Delete(id)
	// A save may have failed again between the checks above and the Delete.
	if _, pending := m.persistPending.Load(id); pending {
		if _, running := m.persistRetrying.LoadOrStore(id, struct{}{}); !running {
			first, _ := m.persistRetryDelays()
			m.schedulePersistRetry(id, first, 1)
		}
	}
}

// persistFileRemoved reports whether a file-backed auth's file no longer exists. A failed save
// leaves the old file in place (see misc.WriteFileAtomic), so a missing file means it was
// removed.
func (m *Manager) persistFileRemoved(id string) bool {
	a, ok := m.GetByID(id)
	if !ok || a == nil || a.Attributes == nil {
		return false
	}
	path := strings.TrimSpace(a.Attributes[AttributePath])
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}
