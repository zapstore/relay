package blossom

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/pippellia-btc/blossy"
)

func TestByteLimit(t *testing.T) {
	exact := &byteLimit{r: strings.NewReader("abcd"), max: 4}
	got, err := io.ReadAll(exact)
	if err != nil || string(got) != "abcd" {
		t.Fatalf("exact: %q %v", got, err)
	}

	over := &byteLimit{r: strings.NewReader("abcde"), max: 4}
	got, err = io.ReadAll(over)
	if !errors.Is(err, errTooLarge) || string(got) != "abcd" {
		t.Fatalf("over: %q %v", got, err)
	}
}

func TestReadOnlyUpload(t *testing.T) {
	err := readOnlyUpload(nil, blossy.UploadHints{})
	if err == nil || err.Code != 403 || err.Reason != "relay is read-only" {
		t.Fatalf("upload: %+v", err)
	}
}

func TestMaxUploadBytes(t *testing.T) {
	if n, ok := maxUploadBytes(apkType); !ok || n != maxAPKBytes {
		t.Fatalf("apk: %d %v", n, ok)
	}
	if n, ok := maxUploadBytes("image/png"); !ok || n != maxImageBytes {
		t.Fatalf("png: %d %v", n, ok)
	}
	if _, ok := maxUploadBytes("image/svg+xml"); ok {
		t.Fatal("svg allowed")
	}
	if _, ok := maxUploadBytes("application/octet-stream"); ok {
		t.Fatal("executable allowed")
	}
}

func TestCollectHashes(t *testing.T) {
	keep := map[string]struct{}{}
	hash := strings.Repeat("ab", 32)
	collectHashes(keep, `[["x","`+hash+`"],["url","https://example"]]`)
	if _, ok := keep[hash]; !ok {
		t.Fatal("missing hash")
	}
	raw := `{"id":"` + strings.Repeat("cd", 32) + `","tags":[["x","` + hash + `"]]}`
	collectHashes(keep, raw)
	if _, ok := keep[strings.Repeat("cd", 32)]; !ok {
		t.Fatal("pending event hash missed")
	}
}
