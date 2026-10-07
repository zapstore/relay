package catalog

import (
	"os"
	"path/filepath"
	"strings"
)

// memo is the apk line of the enricher's cache file.
type memo struct {
	APK string
}

func readMemo(dir string) (memo, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "cache"))
	if err != nil {
		return memo{}, false
	}
	var m memo
	for _, line := range strings.Split(string(raw), "\n") {
		key, val, ok := strings.Cut(line, " ")
		if ok && key == "apk" {
			m.APK = val
		}
	}
	return m, true
}
