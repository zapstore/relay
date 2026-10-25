// Command signers reports APKs that a store signed or delivered, by the rules
// promotion applies (pkg/relay/signers.txt). It changes nothing.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/zapstore/relay/pkg/relay"
)

const usage = `Usage:
  signers <file.apk | dir> ...   Check local APKs; a directory is walked for *.apk
  signers -db relay.db           Check every stored kind 3063, downloading its url

Prints one line per store-signed APK: what was checked, the app id when
known, and the store. The last line counts what was read.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db := flag.String("db", "", "relay.db to read stored assets from")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	if (*db == "") == (flag.NArg() == 0) {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if *db != "" {
		err = checkDB(ctx, *db)
	} else {
		err = checkPaths(flag.Args())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "signers: %v\n", err)
		os.Exit(1)
	}
}

type tally struct{ read, store, failed int }

func (t *tally) check(name, appID string, open func() (io.ReadSeeker, func(), error)) {
	body, cleanup, err := open()
	if err != nil {
		t.failed++
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return
	}
	defer cleanup()
	label, reason, err := relay.StoreSigner(body)
	t.read++
	if err != nil {
		t.failed++
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return
	}
	if label != "" {
		t.store++
		fmt.Printf("%s\t%s\t%s\t%s\n", name, appID, label, reason)
	}
}

func (t tally) print() {
	fmt.Printf("read %d, store-signed %d, failed %d\n", t.read, t.store, t.failed)
}

func checkPaths(paths []string) error {
	var t tally
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || (path != root && !strings.HasSuffix(path, ".apk")) {
				return nil
			}
			t.check(path, "", func() (io.ReadSeeker, func(), error) {
				f, err := os.Open(path)
				if err != nil {
					return nil, nil, err
				}
				return f, func() { f.Close() }, nil
			})
			return nil
		})
		if err != nil {
			return err
		}
	}
	t.print()
	return nil
}

type asset struct{ hash, appID, url string }

func checkDB(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=ro&_busy_timeout=10000")
	if err != nil {
		return err
	}
	defer db.Close()
	assets, err := storedAssets(ctx, db)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	var t tally
	for _, a := range assets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t.check(a.hash, a.appID, func() (io.ReadSeeker, func(), error) {
			return download(ctx, client, a)
		})
	}
	t.print()
	return nil
}

// storedAssets returns one asset per file hash that names an https url.
func storedAssets(ctx context.Context, db *sql.DB) ([]asset, error) {
	rows, err := db.QueryContext(ctx, `SELECT json(tags) FROM events WHERE kind = 3063`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []asset
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var tags [][]string
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			continue
		}
		var a asset
		for _, tag := range tags {
			if len(tag) < 2 {
				continue
			}
			switch {
			case tag[0] == "x" && a.hash == "":
				a.hash = strings.ToLower(tag[1])
			case tag[0] == "i" && a.appID == "":
				a.appID = tag[1]
			case tag[0] == "url" && a.url == "" && strings.HasPrefix(tag[1], "https://"):
				a.url = tag[1]
			}
		}
		if a.hash == "" || a.url == "" || seen[a.hash] {
			continue
		}
		seen[a.hash] = true
		out = append(out, a)
	}
	return out, rows.Err()
}

func download(ctx context.Context, client *http.Client, a asset) (io.ReadSeeker, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET %s: %d", a.url, res.StatusCode)
	}
	f, err := os.CreateTemp("", "signers-*.apk")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), res.Body); err != nil {
		cleanup()
		return nil, nil, err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != a.hash {
		cleanup()
		return nil, nil, errors.New("downloaded file does not match its hash")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, err
	}
	return f, cleanup, nil
}
