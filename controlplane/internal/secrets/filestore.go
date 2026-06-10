package secrets

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
)

// FileStore is the DEV-ONLY secrets backend: a single JSON file on disk,
// mode 0600. It is NOT suitable for production — OpenBao replaces it in Phase 5.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore opens (or initializes) a file-backed secret store at path,
// creating the parent directory if needed. It logs a loud warning so nobody
// mistakes it for a production backend.
func NewFileStore(path string) (*FileStore, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}
	fs := &FileStore{path: path}
	// Touch the file so reads work and permissions are correct from the start.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := fs.writeAll(map[string]map[string]string{}); err != nil {
			return nil, err
		}
	}
	slog.Warn("secrets: using DEV file store — not for production", "path", path)
	return fs, nil
}

func (s *FileStore) Put(path string, data map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return err
	}
	clone := make(map[string]string, len(data))
	maps.Copy(clone, data)
	all[path] = clone
	return s.writeAll(all)
}

func (s *FileStore) Get(path string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	data, ok := all[path]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (s *FileStore) Delete(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return err
	}
	delete(all, path)
	return s.writeAll(all)
}

func (s *FileStore) readAll() (map[string]map[string]string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]map[string]string{}, nil
		}
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	if len(b) == 0 {
		return map[string]map[string]string{}, nil
	}
	var all map[string]map[string]string
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("parse secrets file: %w", err)
	}
	return all, nil
}

func (s *FileStore) writeAll(all map[string]map[string]string) error {
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	// Write atomically via a temp file then rename, preserving 0600.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write secrets file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename secrets file: %w", err)
	}
	return nil
}

// compile-time check
var _ Store = (*FileStore)(nil)
