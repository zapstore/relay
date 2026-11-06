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

// ClearBundles deletes every cached bundle. They are rebuilt from snapshots on
// request, so a bundle on disk is never in the format of an older build.
func ClearBundles(data string) error {
	entries, err := os.ReadDir(BundleDir(data))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(BundleDir(data), entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// fullMarkerPath records that the from→to delta is served as 0→to. The file is empty.
func fullMarkerPath(data string, from, to int64) string {
	return filepath.Join(BundleDir(data), fmt.Sprintf("%d-%d.full", from, to))
}

// pruneBundles deletes the cached bundles and full markers that end before epoch to,
// or start at a snapshot before keepFrom. No request asks for them once to is served.
// A build in progress keeps its temp file.
func pruneBundles(data string, to, keepFrom int64) error {
	entries, err := os.ReadDir(BundleDir(data))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		from, end, ok := bundleSpan(entry.Name())
		if entry.IsDir() || !ok || (end >= to && (from == 0 || from >= keepFrom)) {
			continue
		}
		if err := os.Remove(filepath.Join(BundleDir(data), entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// pruneSnaps deletes the snapshots before epoch keepFrom.
func pruneSnaps(data string, keepFrom int64) error {
	entries, err := os.ReadDir(SnapDir(data))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		n, ok := snapEpoch(entry.Name())
		if entry.IsDir() || !ok || n >= keepFrom {
			continue
		}
		if err := os.Remove(filepath.Join(SnapDir(data), entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// bundleSpan reads from and to from a bundle name, from-to.tar.zst, or a marker, from-to.full.
func bundleSpan(name string) (int64, int64, bool) {
	raw, ok := strings.CutSuffix(name, ".tar.zst")
	if !ok {
		if raw, ok = strings.CutSuffix(name, ".full"); !ok {
			return 0, 0, false
		}
	}
	fromRaw, toRaw, ok := strings.Cut(raw, "-")
	if !ok {
		return 0, 0, false
	}
	from, err := strconv.ParseInt(fromRaw, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	to, err := strconv.ParseInt(toRaw, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return from, to, true
}

// snapEpoch reads n from a snapshot name, n.tar.zst. n is at least 1.
func snapEpoch(name string) (int64, bool) {
	raw, ok := strings.CutSuffix(name, ".tar.zst")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != raw || n < 1 {
		return 0, false
	}
	return n, true
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
		n, ok := snapEpoch(entry.Name())
		if !ok {
			continue
		}
		if n > latest {
			latest = n
		}
	}
	return latest, nil
}
