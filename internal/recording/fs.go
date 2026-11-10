// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// Release does nothing: the directory is the store, and the file stays.
func (s *fsStorage) Release(context.Context, string) error { return nil }

func (s *fsStorage) Pending(ctx context.Context, writtenBefore time.Time) ([]string, error) {
	now := time.Now().UTC()
	days := []string{
		now.AddDate(0, 0, -1).Format("2006/01/02"),
		now.Format("2006/01/02"),
	}
	return s.pendingIn(ctx, writtenBefore, days)
}

// pendingIn lists the recording files under the given dated directories
// (YYYY/MM/DD), or under the whole root when days is nil, whose modification
// time is before the cutoff.
func (s *fsStorage) pendingIn(ctx context.Context, before time.Time, days []string) ([]string, error) {
	var keys []string
	collect := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !e.Type().IsRegular() {
				continue
			}
			rel, err := filepath.Rel(s.root, filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			key := filepath.ToSlash(rel)
			if _, ok := CallIDOf(key); !ok {
				continue
			}
			info, err := e.Info()
			if err != nil || !info.ModTime().Before(before) {
				continue
			}
			keys = append(keys, key)
		}
		return nil
	}
	if days != nil {
		for _, d := range days {
			if err := collect(filepath.Join(s.root, filepath.FromSlash(d))); err != nil {
				return nil, err
			}
		}
		return keys, nil
	}
	// The whole spool: year, month and day directories, nothing deeper.
	years, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for _, y := range years {
		if !y.IsDir() {
			continue
		}
		months, err := os.ReadDir(filepath.Join(s.root, y.Name()))
		if err != nil {
			continue
		}
		for _, m := range months {
			if !m.IsDir() {
				continue
			}
			ds, err := os.ReadDir(filepath.Join(s.root, y.Name(), m.Name()))
			if err != nil {
				continue
			}
			for _, d := range ds {
				if !d.IsDir() {
					continue
				}
				if err := collect(filepath.Join(s.root, y.Name(), m.Name(), d.Name())); err != nil {
					return nil, err
				}
			}
		}
	}
	return keys, nil
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
