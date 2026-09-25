package blossom

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errChecksumMismatch = errors.New("checksum mismatch")

// Files stores blob bytes on disk when Bunny is unset.
type Files struct {
	Dir string
}

func (f Files) resolve(rel string) (string, error) {
	if f.Dir == "" {
		return "", errors.New("blob directory is not set")
	}
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("invalid blob path")
	}
	return filepath.Join(f.Dir, rel), nil
}

func (f Files) put(rel string, r io.Reader, want []byte) (int64, error) {
	dest, err := f.resolve(rel)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	closeErr := tmp.Close()
	if err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return 0, closeErr
	}
	if want != nil && !bytes.Equal(hasher.Sum(nil), want) {
		os.Remove(tmpName)
		return 0, errChecksumMismatch
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	return n, nil
}

func (f Files) open(rel string) (*os.File, error) {
	path, err := f.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}
