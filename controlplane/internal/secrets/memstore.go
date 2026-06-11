package secrets

import (
	"maps"
	"sync"
)

// MemStore is an in-memory Store. It is used by tests (and could back a fully
// ephemeral dev mode), keeping secret material off disk entirely.
type MemStore struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

// NewMemStore returns an empty in-memory secret store.
func NewMemStore() *MemStore {
	return &MemStore{data: map[string]map[string]string{}}
}

func (s *MemStore) Put(path string, data map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := make(map[string]string, len(data))
	maps.Copy(clone, data)
	s.data[path] = clone
	return nil
}

func (s *MemStore) Get(path string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data[path]
	if !ok {
		return nil, ErrNotFound
	}
	clone := make(map[string]string, len(d))
	maps.Copy(clone, d)
	return clone, nil
}

func (s *MemStore) Delete(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, path)
	return nil
}

var _ Store = (*MemStore)(nil)
