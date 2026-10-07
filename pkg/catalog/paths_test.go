package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAppDirDeletesTheArtifactDir(t *testing.T) {
	data := t.TempDir()
	dir := AppDir(data, "com.example")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := AppDir(data, "com.other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeAppDir(data, "com.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAppDirRejectsAPath(t *testing.T) {
	if err := removeAppDir(t.TempDir(), "../x"); err == nil {
		t.Fatal("accepted ..")
	}
	if err := removeAppDir(t.TempDir(), "a/b"); err == nil {
		t.Fatal("accepted slash")
	}
}
