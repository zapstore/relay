package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupBinaryKeepsTheSymlink(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "releases", "relay-old")
	if err := os.MkdirAll(filepath.Dir(release), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "relay")
	rel, err := filepath.Rel(binDir, release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, link); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(root, "backup", backupRelayBinary)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backupBinary(t.Context(), link, backup); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(backup)
	if err != nil || got != rel {
		t.Fatalf("backup link = %q, %v, want %q", got, err, rel)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("replaced"), 0o755); err != nil {
		t.Fatal(err)
	}

	aside := link + ".replaced"
	if err := swapIn(t.Context(), backup, link, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(aside); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != rel {
		t.Fatalf("restored link = %q, %v, want %q", got, err, rel)
	}
	if b, _ := os.ReadFile(release); string(b) != "old" {
		t.Fatal("release changed during restore")
	}
	if b, _ := os.ReadFile(link); string(b) != "old" {
		t.Fatalf("relay via the link = %q", b)
	}
}

func TestSwapInReplacesAFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "relay.env")
	src := filepath.Join(dir, "backup.env")
	if err := os.WriteFile(dst, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	aside := dst + ".replaced"
	if err := swapIn(t.Context(), src, dst, aside); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "backup" {
		t.Fatalf("restored = %q, %v", got, err)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Fatal(err)
	}
}

func TestBackupBinaryRejectsADanglingLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "relay")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if err := backupBinary(t.Context(), link, filepath.Join(dir, "out")); err == nil {
		t.Fatal("a dangling symlink was backed up")
	}
}

func TestRollbackPutsTheOldTreeBack(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "data")
	aside := dst + ".replaced"
	for _, path := range []string{dst, aside} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "keep"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aside, "keep"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := rollback(dst, aside, errors.New("copy failed"))
	if err == nil || !strings.Contains(err.Error(), "copy failed") {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "keep"))
	if err != nil || string(got) != "old" {
		t.Fatalf("data = %q, %v", got, err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatal("aside was left behind")
	}
}
