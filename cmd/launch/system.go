package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func systemctl(ctx context.Context, t term, verb, unit string) error {
	t.say("systemctl %s %s", verb, unit)
	if out, err := exec.CommandContext(ctx, "systemctl", verb, unit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s %s: %s", verb, unit, firstLine(out, err))
	}
	return nil
}

// stopAll stops public traffic first, then the writers.
// A unit that does not stop is started again, with anything already stopped.
func stopAll(ctx context.Context, t term, s site) error {
	return stopList(ctx, t, []string{s.ProxyUnit, s.IndexerUnit, s.RelayUnit})
}

// stopList stops units in order. A unit that does not stop is left, and
// anything already stopped is started again.
func stopList(ctx context.Context, t term, units []string) error {
	var stopped []string
	for _, unit := range units {
		if err := systemctl(ctx, t, "stop", unit); err != nil {
			if len(stopped) > 0 {
				t.say("Stop failed. Starting what was stopped.")
				err = errors.Join(err, startUnits(t, reverse(stopped)))
			}
			return err
		}
		stopped = append(stopped, unit)
	}
	return nil
}

// hold runs fn with the proxy, the indexer, and the relay stopped. If fn
// returns before changing anything, the units are started again. The caller's
// context may already be cancelled; the restart uses its own.
func hold(ctx context.Context, t term, s site, fn func() (bool, error)) error {
	return holdUnits(ctx, t,
		[]string{s.ProxyUnit, s.IndexerUnit, s.RelayUnit},
		[]string{s.RelayUnit, s.IndexerUnit, s.ProxyUnit},
		fn)
}

// holdUnits stops stop, runs fn, and starts restart when fn changed nothing.
func holdUnits(ctx context.Context, t term, stop, restart []string, fn func() (bool, error)) error {
	if err := stopList(ctx, t, stop); err != nil {
		return err
	}
	changed, err := fn()
	if err == nil || changed {
		return err
	}
	t.say("Nothing was changed. Starting the services again.")
	if startErr := startUnits(t, restart); startErr != nil {
		return errors.Join(err, startErr)
	}
	return err
}

func startAll(t term, s site) error {
	return startUnits(t, []string{s.RelayUnit, s.IndexerUnit, s.ProxyUnit})
}

// startUnits starts units on a fresh context. Shutdown of the caller must
// not leave the stopped units down.
func startUnits(t term, units []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var err error
	for _, unit := range units {
		err = errors.Join(err, systemctl(ctx, t, "start", unit))
	}
	return err
}

func reverse(units []string) []string {
	out := make([]string, len(units))
	for i, unit := range units {
		out[len(units)-1-i] = unit
	}
	return out
}

// undone is a failure after the backup has been used. The services stay stopped.
func undone(backup string, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	return true, fmt.Errorf("%w\nUndo with: launch restore %s", err, backup)
}

// databaseFiles are the relay and blossom databases, including SQLite sidecars.
// indexer.db is not one of them: the indexer and the relay share a data directory.
var databaseFiles = []string{
	"relay.db", "relay.db-wal", "relay.db-shm",
	"blossom.db", "blossom.db-wal", "blossom.db-shm",
}

// backupFile is one live database file stored in a backup directory.
type backupFile struct {
	Name string `json:"name"`
	Live string `json:"live"`
}

// backupInfo is written into every backup so restore knows where things go.
type backupInfo struct {
	Site  site         `json:"site"`
	Paths paths        `json:"paths"`
	Files []backupFile `json:"files,omitempty"`
}

const (
	backupRelayData   = "relay-data"
	backupIndexerData = "indexer-data"
	backupRelayEnv    = "relay.env"
	backupIndexerEnv  = "indexer.env"
	backupRelayBinary = "relay-binary"
	backupInfoFile    = "backup.json"
)

// takeBackup copies the relay data, the indexer data, both env files, and the relay binary.
func takeBackup(ctx context.Context, t term, s site, p paths, window string) (string, error) {
	dir := filepath.Join(s.BackupRoot, window+"-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	t.say("Backing up to %s", dir)
	copies := []struct{ src, dst string }{
		{p.RelayData, backupRelayData},
		{p.IndexerData, backupIndexerData},
		{s.RelayEnv, backupRelayEnv},
		{s.IndexerEnv, backupIndexerEnv},
		{s.RelayBinary, backupRelayBinary},
	}
	for _, c := range copies {
		var err error
		if c.dst == backupRelayBinary {
			err = backupBinary(ctx, c.src, filepath.Join(dir, c.dst))
		} else {
			err = copyTree(ctx, c.src, filepath.Join(dir, c.dst))
		}
		if err != nil {
			return "", fmt.Errorf("back up %s: %w", c.src, err)
		}
	}
	if err := writeJSON(filepath.Join(dir, backupInfoFile), backupInfo{Site: s, Paths: p}); err != nil {
		return "", err
	}
	if err := syncTree(dir); err != nil {
		return "", fmt.Errorf("sync backup: %w", err)
	}
	t.say("Backup done. Undo this window with: launch restore %s", dir)
	t.say("Not in the backup: APKs on Bunny (safe while BLOSSOM_PRUNE_INTERVAL=0) and the")
	t.say("indexer's zsp HTTP cache (<indexer user's cache dir>/zsp).")
	return dir, nil
}

// ownerOf is the uid and gid of path when running as root. Otherwise chown is skipped.
func ownerOf(path string) (int, int) {
	info, err := os.Stat(path)
	if err != nil || os.Geteuid() != 0 {
		return -1, -1
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}

// restoreDatabaseFiles puts a database backup back. The relay stays stopped.
func restoreDatabaseFiles(ctx context.Context, dir string) error {
	info, err := readBackup(dir)
	if err != nil {
		return err
	}
	if info.Paths.RelayData == "" {
		return fmt.Errorf("%s has no relay data path", dir)
	}
	for _, name := range databaseFiles {
		dst := filepath.Join(info.Paths.RelayData, name)
		if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", dst, err)
		}
	}
	uid, gid := ownerOf(info.Paths.RelayData)
	for _, f := range info.Files {
		src := filepath.Join(dir, f.Name)
		if err := copyTree(ctx, src, f.Live); err != nil {
			return fmt.Errorf("restore %s: %w", f.Live, err)
		}
		if err := os.Chmod(f.Live, 0o644); err != nil {
			return err
		}
		if uid >= 0 {
			if err := os.Chown(f.Live, uid, gid); err != nil {
				return fmt.Errorf("chown %s: %w", f.Live, err)
			}
		}
	}
	return syncPath(info.Paths.RelayData)
}

func restoreDatabases(ctx context.Context, t term, dir string, info backupInfo) error {
	t.say("This puts the relay databases back in %s.", info.Paths.RelayData)
	start := []string{info.Site.RelayUnit}
	stop := []string{info.Site.RelayUnit}
	if info.Site.OldUnit != "" {
		t.say("It also starts %s.", info.Site.OldUnit)
		stop = []string{info.Site.OldUnit, info.Site.RelayUnit}
		start = []string{info.Site.RelayUnit, info.Site.OldUnit}
	}
	if err := t.confirm(ctx, "Stop the relay and restore the databases?"); err != nil {
		return err
	}
	if err := stopList(ctx, t, stop); err != nil {
		return err
	}
	if err := restoreDatabaseFiles(ctx, dir); err != nil {
		return err
	}
	t.say("Restored the databases in %s.", info.Paths.RelayData)
	question := "Start the relay again?"
	if info.Site.OldUnit != "" {
		question = "Start the relay and " + info.Site.OldUnit + " again?"
	}
	if err := t.confirm(ctx, question); err != nil {
		return err
	}
	return startUnits(t, start)
}

func readBackup(dir string) (backupInfo, error) {
	raw, err := os.ReadFile(filepath.Join(dir, backupInfoFile))
	if err != nil {
		return backupInfo{}, fmt.Errorf("%s is not a launch backup: %w", dir, err)
	}
	var info backupInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return backupInfo{}, fmt.Errorf("read %s: %w", backupInfoFile, err)
	}
	return info, nil
}

func restore(ctx context.Context, t term, dir string) error {
	info, err := readBackup(dir)
	if err != nil {
		return err
	}
	if len(info.Files) > 0 {
		return restoreDatabases(ctx, t, dir, info)
	}
	s, p := info.Site, info.Paths
	t.say("This puts back, from %s:", dir)
	t.say("  %s", p.RelayData)
	t.say("  %s", p.IndexerData)
	t.say("  %s, %s, %s", s.RelayEnv, s.IndexerEnv, s.RelayBinary)
	t.say("The current data directories are kept beside them with a .replaced suffix.")
	if err := t.confirm(ctx, "Stop the services and restore?"); err != nil {
		return err
	}
	if err := stopAll(ctx, t, s); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	for _, d := range []struct{ src, dst string }{
		{backupRelayData, p.RelayData},
		{backupIndexerData, p.IndexerData},
	} {
		aside := d.dst + ".replaced-" + stamp
		if err := swapIn(ctx, filepath.Join(dir, d.src), d.dst, aside); err != nil {
			return err
		}
		t.say("Restored %s. The replaced copy is %s", d.dst, aside)
	}
	for _, f := range []struct{ src, dst string }{
		{backupRelayEnv, s.RelayEnv},
		{backupIndexerEnv, s.IndexerEnv},
		{backupRelayBinary, s.RelayBinary},
	} {
		aside := f.dst + ".replaced-" + stamp
		if err := swapIn(ctx, filepath.Join(dir, f.src), f.dst, aside); err != nil {
			return err
		}
		if err := os.RemoveAll(aside); err != nil {
			return err
		}
	}
	if err := t.wait(ctx,
		"If the indexer ran after this backup, clear its zsp HTTP cache",
		"(<indexer user's cache dir>/zsp, for example ~/.cache/zsp).",
		"It remembers releases this restore removed, and would not republish",
		"them until their source changes. Skip this if the indexer has not run.",
	); err != nil {
		return err
	}
	if err := t.confirm(ctx, "Start the relay, the indexer, and the proxy?"); err != nil {
		return err
	}
	for _, unit := range []string{s.RelayUnit, s.IndexerUnit, s.ProxyUnit} {
		if err := systemctl(ctx, t, "start", unit); err != nil {
			return err
		}
	}
	return nil
}

// backupBinary copies the relay binary. A symlink is stored as a symlink
// after its target is checked, so restore can put the link back.
func backupBinary(ctx context.Context, src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		resolved := target
		if !filepath.IsAbs(target) {
			resolved = filepath.Join(filepath.Dir(src), target)
		}
		st, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("%s points at %s: %w", src, target, err)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s points at %s, which is not a file", src, target)
		}
	}
	return copyTree(ctx, src, dst)
}

func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		return syncPath(path)
	})
}

// swapIn moves dst aside and puts src in its place. A failed copy puts dst back.
func swapIn(ctx context.Context, src, dst, aside string) error {
	if err := os.Rename(dst, aside); err != nil {
		return err
	}
	if err := copyTree(ctx, src, dst); err != nil {
		return rollback(dst, aside, err)
	}
	if err := syncTree(dst); err != nil {
		return rollback(dst, aside, err)
	}
	if err := syncPath(filepath.Dir(dst)); err != nil {
		return rollback(dst, aside, err)
	}
	return nil
}

func rollback(dst, aside string, err error) error {
	if rmErr := os.RemoveAll(dst); rmErr != nil {
		return fmt.Errorf("restore %s: %w (the replaced copy is %s; removing the partial copy: %v)", dst, err, aside, rmErr)
	}
	if mvErr := os.Rename(aside, dst); mvErr != nil {
		return fmt.Errorf("restore %s: %w (the replaced copy is %s and was not moved back: %v)", dst, err, aside, mvErr)
	}
	return fmt.Errorf("restore %s: %w", dst, err)
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	return errors.Join(err, closeErr)
}

// copyTree copies a file or a directory, keeping modes, owners, and times.
// Sockets and other special files are skipped: a stopped relay leaves no live ones.
func copyTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return err
			}
			if err := os.Chmod(target, mode.Perm()); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target) // a missing target is the usual case
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			return keepOwner(target, info, true)
		case mode.IsRegular():
			if err := copyFile(path, target, mode.Perm()); err != nil {
				return err
			}
		default:
			return nil
		}
		if err := keepOwner(target, info, false); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".launch-copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	done = true
	return nil
}

// keepOwner copies the owner when running as root. Other users cannot chown.
func keepOwner(path string, info fs.FileInfo, link bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	if link {
		return os.Lchown(path, int(st.Uid), int(st.Gid))
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
