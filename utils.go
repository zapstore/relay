package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func makePlaceHolders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func mkdir(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("could not create directory %s", path)
	}

	return nil
}

func writeFile(filename string, data []byte) error {
	if err := mkdir(filepath.Dir(filename)); err != nil {
		return err
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		return fmt.Errorf("failed to write to %s: %w", filename, err)
	}

	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}

	return err == nil
}
