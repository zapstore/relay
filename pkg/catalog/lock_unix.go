//go:build unix

package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockSeal holds seal.lock in the data directory until unlock. It keeps a
// `relay bundle` process and a running relay from writing the same epoch.
func lockSeal(data string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(data, "seal.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("seal lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("seal lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
