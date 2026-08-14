// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// fsStorage keeps recordings where the switch wrote them.
type fsStorage struct{ root string }

func newFSStorage(root string) *fsStorage { return &fsStorage{root: root} }

func (s *fsStorage) Backend() string { return "FS" }
func (s *fsStorage) Bucket() string  { return "" }

// resolve joins the key under the root and refuses to escape it: keys come
// from the database, but a path that walks out of the recordings directory
// must be impossible rather than merely unexpected.
func (s *fsStorage) resolve(key string) (string, error) {
	cleaned := filepath.Clean(filepath.Join(s.root, filepath.FromSlash(key)))
	root := filepath.Clean(s.root) + string(os.PathSeparator)
	if !strings.HasPrefix(cleaned, root) {
		return "", fmt.Errorf("recording: key %q escapes the recording directory", key)
	}
	return cleaned, nil
}

func (s *fsStorage) Ingest(_ context.Context, key string) (int64, error) {
	full, err := s.resolve(key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *fsStorage) Open(_ context.Context, key string) (io.ReadSeekCloser, int64, error) {
	full, err := s.resolve(key)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(full)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func (s *fsStorage) Delete(_ context.Context, key string) error {
	full, err := s.resolve(key)
	if err != nil {
		return err
	}
	err = os.Remove(full)
	if os.IsNotExist(err) {
		return nil // already gone is the outcome deletion wanted
	}
	return err
}
