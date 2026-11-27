package runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/8iops/stalwart-agent-tools/internal/groupware"
)

type responseEntry struct {
	fingerprint string
	done        chan struct{}
	result      groupware.ResponseResult
	err         error
}
type memoryResponseStore struct {
	mu      sync.Mutex
	entries map[string]*responseEntry
}

func newMemoryResponseStore() *memoryResponseStore {
	return &memoryResponseStore{entries: map[string]*responseEntry{}}
}
func (s *memoryResponseStore) Do(ctx context.Context, key, fingerprint string, operation func(context.Context) (groupware.ResponseResult, error)) (groupware.ResponseResult, error) {
	s.mu.Lock()
	entry, exists := s.entries[key]
	if exists {
		if entry.fingerprint != fingerprint {
			s.mu.Unlock()
			return groupware.ResponseResult{}, errors.New("idempotency key belongs to another calendar response")
		}
		done := entry.done
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return groupware.ResponseResult{}, ctx.Err()
		case <-done:
			return entry.result, entry.err
		}
	}
	entry = &responseEntry{fingerprint: fingerprint, done: make(chan struct{})}
	s.entries[key] = entry
	s.mu.Unlock()
	entry.result, entry.err = operation(ctx)
	s.mu.Lock()
	close(entry.done)
	s.mu.Unlock()
	return entry.result, entry.err
}
