package main

import (
	"context"
	"encoding/json"
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
func stopAll(ctx context.Context, t term, s site) error {
	for _, unit := range []string{s.ProxyUnit, s.IndexerUnit, s.RelayUnit} {
		if err := systemctl(ctx, t, "stop", unit); err != nil {
			return err
		}
	}
	return nil
}

// backupInfo is written into every backup so restore knows where things go.
type backupInfo struct {
	Site  site  `json:"site"`
	Paths paths `json:"paths"`
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
func takeBackup(t term, s site, p paths, window string) (string, error) {
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
		if err := copyTree(c.src, filepath.Join(dir, c.dst)); err != nil {
			return "", fmt.Errorf("back up %s: %w", c.src, err)
		}
	}
	if err := writeJSON(filepath.Join(dir, backupInfoFile), backupInfo{Site: s, Paths: p}); err != nil {
		return "", err
	}
	t.say("Backup done. Undo this window with: launch restore %s", dir)
	t.say("Not in the backup: APKs on Bunny (safe while PRUNE_INTERVAL=0) and the")
	t.say("indexer's zsp HTTP cache (<indexer user's cache dir>/zsp).")
	return dir, nil
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
	s, p := info.Site, info.Paths
	t.say("This puts back, from %s:", dir)
	t.say("  %s", p.RelayData)
	t.say("  %s", p.IndexerData)
	t.say("  %s, %s, %s", s.RelayEnv, s.IndexerEnv, s.RelayBinary)
	t.say("The current data directories are kept beside them with a .replaced suffix.")
	if err := t.confirm("Stop the services and restore?"); err != nil {
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
		if err := os.Rename(d.dst, aside); err != nil {
			return err
		}
		if err := copyTree(filepath.Join(dir, d.src), d.dst); err != nil {
			return fmt.Errorf("restore %s: %w", d.dst, err)
		}
		t.say("Restored %s. The replaced copy is %s", d.dst, aside)
	}
	for _, f := range []struct{ src, dst string }{
		{backupRelayEnv, s.RelayEnv},
		{backupIndexerEnv, s.IndexerEnv},
		{backupRelayBinary, s.RelayBinary},
	} {
		if err := copyTree(filepath.Join(dir, f.src), f.dst); err != nil {
			return fmt.Errorf("restore %s: %w", f.dst, err)
		}
	}
	if err := t.wait(
		"If the indexer ran after this backup, clear its zsp HTTP cache",
		"(<indexer user's cache dir>/zsp, for example ~/.cache/zsp).",
		"It remembers releases this restore removed, and would not republish",
		"them until their source changes. Skip this if the indexer has not run.",
	); err != nil {
		return err
	}
	if err := t.confirm("Start the relay, the indexer, and the proxy?"); err != nil {
		return err
	}
	for _, unit := range []string{s.RelayUnit, s.IndexerUnit, s.ProxyUnit} {
		if err := systemctl(ctx, t, "start", unit); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies a file or a directory, keeping modes, owners, and times.
// Sockets and other special files are skipped: a stopped relay leaves no live ones.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
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
