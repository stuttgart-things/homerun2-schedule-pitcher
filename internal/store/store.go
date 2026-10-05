// Package store keeps check state between runs.
package store

import (
	"context"
	"sync"
	"time"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/state"
)

// Store persists check state and serialises runs of the same check.
type Store interface {
	// Get returns the state of a check, or state.New(id) if none is stored.
	Get(ctx context.Context, id string) (state.State, error)
	Put(ctx context.Context, s state.State) error
	// TryLock takes the run lock of a check for at most ttl. It returns false
	// if another run holds it.
	TryLock(ctx context.Context, id string, ttl time.Duration) (bool, error)
	Unlock(ctx context.Context, id string) error
}

// Memory is the no-Redis store: state lives in the process and is lost on
// restart, so a check that is not ok is pitched once more after a restart.
type Memory struct {
	mu     sync.Mutex
	states map[string]state.State
	locks  map[string]time.Time
	now    func() time.Time
}

func NewMemory() *Memory {
	return &Memory{states: map[string]state.State{}, locks: map[string]time.Time{}, now: time.Now}
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
