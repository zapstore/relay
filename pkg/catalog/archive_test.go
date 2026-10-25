package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteAtomicConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1-2.tar.zst")
	bodies := [][]byte{
		bytes.Repeat([]byte("a"), 256*1024),
		bytes.Repeat([]byte("b"), 256*1024),
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, body := range bodies {
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			<-start
			if err := writeAtomic(path, body); err != nil {
				t.Error(err)
			}
		}(body)
	}
	close(start)
	wg.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bodies[0]) && !bytes.Equal(got, bodies[1]) {
		t.Fatalf("bundle length %d is a mix of the two writes", len(got))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d dir entries, want only the bundle", len(entries))
	}
}
