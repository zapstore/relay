package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func SnapDir(data string) string     { return filepath.Join(data, "snapshots") }
func BundleDir(data string) string   { return filepath.Join(data, "bundles") }
func ArtifactDir(data string) string { return filepath.Join(data, "artifacts") }

func AppDir(data, appID string) string {
	return filepath.Join(ArtifactDir(data), appID)
}

// removeAppDir deletes one artifact directory. The id must be a single path segment.
func removeAppDir(data, appID string) error {
	if appID == "" || appID != filepath.Base(appID) || strings.Contains(appID, "..") {
		return fmt.Errorf("app id %q", appID)
	}
	return os.RemoveAll(AppDir(data, appID))
}

// avatarFilename is <64 hex characters>.webp, on disk and in the bundle.
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
