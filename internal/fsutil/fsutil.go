// Package fsutil holds file helpers shared by the packages that rewrite files
// other programs read, and the lock every shared read-modify-write takes.
package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temporary file in the same directory and
// renames it over path, so readers see either the old or the new content.
// A symlink at path is followed, so a dotfiles link stays a link.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	path, err := linkTarget(path)
	if err != nil {
		return err
	}
	tmp, err := writeTemp(path, data, mode)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Rename(tmp, path)
}

// writeTemp writes data with mode to a new temporary file beside path and
// returns its name.
func writeTemp(path string, data []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".hrk-*")
	if err != nil {
		return "", err
	}
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// linkTarget follows symlinks, including one whose target does not exist
// yet, so writing never replaces a link with a plain file.
func linkTarget(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	return "", fmt.Errorf("%s: too many symlinks", path)
}
