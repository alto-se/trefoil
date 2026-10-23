package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotFound is returned by ReadPath when nothing is stored at the path.
var ErrNotFound = errors.New("not found")

// Store reads and writes blobs by path. Swap the implementation to move
// storage to an external backend (S3, GCS, ...) without touching handlers.
type Store interface {
	WritePath(ctx context.Context, path string, data []byte) error
	ReadPath(ctx context.Context, path string) ([]byte, error)
}

// Local stores files under a root directory on disk.
type Local struct {
	root string
}

func NewLocal(root string) *Local {
	return &Local{root: root}
}

func (l *Local) WritePath(ctx context.Context, path string, data []byte) error {
	full, err := l.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}

	// Write to a temp file and rename so readers never see a partial file.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

func (l *Local) ReadPath(ctx context.Context, path string) ([]byte, error) {
	full, err := l.resolve(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

// resolve maps path to a file under root, rejecting paths that escape it.
func (l *Local) resolve(path string) (string, error) {
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("invalid path %q", path)
	}
	return filepath.Join(l.root, path), nil
}
