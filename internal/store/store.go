// Package store keeps check state between runs.
package store

import (
	"context"
	"sync"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// HistoryLimit is how many runs are kept per check.
const HistoryLimit = 100

// Store persists check state and serialises runs of the same check.
type Store interface {
	// Get returns the state of a check, or state.New(id) if none is stored.
	Get(ctx context.Context, id string) (state.State, error)
	Put(ctx context.Context, s state.State) error
	// AddHistory records one run; only the last HistoryLimit runs are kept.
	AddHistory(ctx context.Context, id string, e HistoryEntry) error
	// History returns up to limit runs, newest first.
	History(ctx context.Context, id string, limit int) ([]HistoryEntry, error)
	// TryLock takes the run lock of a check for at most ttl. It returns false
	// if another run holds it.
	TryLock(ctx context.Context, id string, ttl time.Duration) (bool, error)
	Unlock(ctx context.Context, id string) error
	// Ping reports whether the store is usable.
	Ping(ctx context.Context) error
}

// HistoryEntry is the outcome of one run.
type HistoryEntry struct {
	At      time.Time   `json:"at"`
	Band    status.Band `json:"band"`
	Failing bool        `json:"failing"`
	Expiry  time.Time   `json:"expiry,omitzero"`
	Summary string      `json:"summary,omitempty"`
	Problem string      `json:"problem,omitempty"`
	Error   string      `json:"error,omitempty"`
	// Pitched lists the kinds of the notifications delivered by this run.
	Pitched []string `json:"pitched,omitempty"`
}

// EntryFor builds the history entry of a run that ended in s.
func EntryFor(s state.State, pitched []string) HistoryEntry {
	e := HistoryEntry{At: s.LastRun, Band: s.Band, Failing: s.Failing, Pitched: pitched}
	if s.Failing {
		e.Error = s.LastError
		return e
	}
	e.Expiry, e.Summary, e.Problem = s.Expiry, s.Summary, s.Problem
	return e
}

// Memory is the no-Redis store: state lives in the process and is lost on
// restart, so a check that is not ok is pitched once more after a restart.
type Memory struct {
	mu      sync.Mutex
	states  map[string]state.State
	history map[string][]HistoryEntry
	locks   map[string]time.Time
	now     func() time.Time
}

func NewMemory() *Memory {
	return &Memory{
		states:  map[string]state.State{},
		history: map[string][]HistoryEntry{},
		locks:   map[string]time.Time{},
		now:     time.Now,
	}
}

func (m *Memory) Get(_ context.Context, id string) (state.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.states[id]; ok {
		return s, nil
	}
	return state.New(id), nil
}

func (m *Memory) Put(_ context.Context, s state.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[s.CheckID] = s
	return nil
}

func (m *Memory) AddHistory(_ context.Context, id string, e HistoryEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := append(m.history[id], e)
	if len(h) > HistoryLimit {
		h = h[len(h)-HistoryLimit:]
	}
	m.history[id] = h
	return nil
}

func (m *Memory) History(_ context.Context, id string, limit int) ([]HistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.history[id]
	out := make([]HistoryEntry, 0, min(limit, len(h)))
	for i := len(h) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, h[i])
	}
	return out, nil
}

func (m *Memory) TryLock(_ context.Context, id string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if until, ok := m.locks[id]; ok && now.Before(until) {
		return false, nil
	}
	m.locks[id] = now.Add(ttl)
	return true, nil
}

func (m *Memory) Unlock(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, id)
	return nil
}

func (m *Memory) Ping(context.Context) error { return nil }
