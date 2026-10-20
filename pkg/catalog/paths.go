package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func SnapDir(data string) string   { return filepath.Join(data, "snapshots") }
func BundleDir(data string) string { return filepath.Join(data, "bundles") }

// avatarFilename is <64 hex characters>.webp in the bundle.
func avatarFilename(pubkey string) (string, error) {
	if len(pubkey) != 64 {
		return "", fmt.Errorf("avatar pubkey")
	}
	for _, c := range pubkey {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return "", fmt.Errorf("avatar pubkey")
		}
	}
	return pubkey + ".webp", nil
}

func isAvatarFilename(name string) bool {
	pubkey, ok := strings.CutSuffix(name, ".webp")
	if !ok || strings.Contains(name, "/") {
		return false
	}
	_, err := avatarFilename(pubkey)
	return err == nil
}

func SnapPath(data string, n int64) string {
	return filepath.Join(SnapDir(data), fmt.Sprintf("%d.tar.zst", n))
}

func BundlePath(data string, from, to int64) string {
	return filepath.Join(BundleDir(data), fmt.Sprintf("%d-%d.tar.zst", from, to))
}

func LatestSnap(data string) (int64, error) {
	entries, err := os.ReadDir(SnapDir(data))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var latest int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".tar.zst") {
			continue
		}
		raw := strings.TrimSuffix(name, ".tar.zst")
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != raw || n < 1 {
			continue
		}
		if n > latest {
			latest = n
		}
	}
	return latest, nil
}

// FloorPath holds the highest epoch this data directory has written. It sits outside
// snapshots/ so that clearing snapshots for a rebuild keeps epochs climbing.
func FloorPath(data string) string { return filepath.Join(data, "epoch-floor") }

// ReadFloor returns the stored floor, or 0 when there is none.
func ReadFloor(data string) (int64, error) {
	raw, err := os.ReadFile(FloorPath(data))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	text := strings.TrimSpace(string(raw))
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("epoch floor %q", text)
	}
	return n, nil
}

func writeFloor(data string, n int64) error {
	return writeAtomic(FloorPath(data), []byte(strconv.FormatInt(n, 10)+"\n"))
}
