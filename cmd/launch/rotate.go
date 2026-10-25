package main

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/catalog"
)

// rotate is window 2: the new catalog key.
func rotate(ctx context.Context, t term) error {
	s, err := loadSite(ctx, t)
	if err != nil {
		return err
	}
	t.say("\nWindow 2: the new catalog key.")
	if err := t.confirm(ctx, "Is relay v2 live on 78ce…?"); err != nil {
		return err
	}
	if err := t.confirm(ctx, "Is the new key in hotbox, and does the released 1.1.3 trust it?"); err != nil {
		return err
	}
	if err := t.confirm(ctx, "Did launch rotate --dry-run pass on a recent backup?"); err != nil {
		return err
	}
	p, err := s.paths(ctx)
	if err != nil {
		return err
	}
	if err := t.confirm(ctx, fmt.Sprintf("Stop %s, %s, and %s now?", s.ProxyUnit, s.IndexerUnit, s.RelayUnit)); err != nil {
		return err
	}
	return hold(ctx, t, s, func() (bool, error) {
		backup, err := takeBackup(ctx, t, s, p, "rotate")
		if err != nil {
			return false, err
		}
		pl, err := prepare(ctx, t, p.RelayData, p.indexerDB(), backup)
		if err != nil {
			return false, err
		}
		if err := t.confirm(ctx, fmt.Sprintf("Delete those %d events and the snapshots and bundles?", len(pl.Delete))); err != nil {
			return false, err
		}
		newKey, signer, err := editRelayEnv(ctx, t, s.RelayEnv, relayEnvWant{intervals: true},
			"RELAY_PUBKEY=<the new key>",
			"SIGN_WITH=<the new key's bunker URL>",
		)
		if err != nil {
			return undone(backup, err)
		}
		if err := editIndexerEnv(ctx, t, s.IndexerEnv, newKey); err != nil {
			return undone(backup, err)
		}
		lud16, err := t.ask(ctx, "lud16 for the new key's kind 0", lud16Of(pl.Profile))
		if err != nil {
			return undone(backup, err)
		}
		evs, err := resign(ctx, pl, signer.Sign, newKey, lud16, time.Now())
		if err != nil {
			return undone(backup, err)
		}
		if err := clearOld(ctx, t, p.RelayData, pl); err != nil {
			return undone(backup, err)
		}
		rep, err := storeAll(ctx, t, p.RelayData, evs)
		rep.print(t)
		if err != nil {
			return undone(backup, err)
		}

		p, err = s.paths(ctx)
		if err != nil {
			return undone(backup, err)
		}
		if err := systemctl(ctx, t, "start", s.RelayUnit); err != nil {
			return undone(backup, err)
		}
		if err := waitRelay(ctx, t, p.RelayAddr, newKey); err != nil {
			return undone(backup, err)
		}
		if err := waitSocket(ctx, t, p.RelaySocket); err != nil {
			return undone(backup, err)
		}
		sock := &nostrSocket{path: p.RelaySocket}
		defer sock.Close()
		if err := checkRotation(ctx, t, sock, newKey, pl, rep); err != nil {
			return undone(backup, err)
		}
		for _, unit := range []string{s.IndexerUnit, s.ProxyUnit} {
			if err := systemctl(ctx, t, "start", unit); err != nil {
				return undone(backup, err)
			}
		}
		t.say("\nWindow 2 is done. The catalog key is %s.", newKey)
		t.say("Undo with: launch restore %s", backup)
		t.say("Once prune has run, the APKs of the deleted listings are gone from Bunny,")
		t.say("and a restore brings back their events without their files.")
		t.say("\nThe indexer:")
		t.say("  - publishes under the new key only when a source changes; its zsp HTTP")
		t.say("    cache still says unchanged for the re-signed apps, so nothing is republished.")
		t.say("  - does not ask the relay what is published. The relay's version guard,")
		t.say("    on the version codes the re-sign kept, is what blocks a downgrade.")
		if len(pl.Dropped) > 0 {
			t.say("  - will not bring back the %d dropped apps until their source changes", len(pl.Dropped))
			t.say("    or a fetch fails. Run the indexer on the ids in %s.", filepath.Join(backup, droppedFile))
		}
		return true, nil
	})
}

// prepare reads 78ce…'s events from a stopped relay and writes the plan into dir.
func prepare(ctx context.Context, t term, relayData, indexerDB, dir string) (plan, error) {
	relayDB, err := openDB(ctx, filepath.Join(relayData, "relay.db"), false)
	if err != nil {
		return plan{}, err
	}
	defer relayDB.Close()
	old, err := eventsBy(ctx, relayDB, oldKey)
	if err != nil {
		return plan{}, err
	}
	pending, err := pendingBy(ctx, relayDB, oldKey)
	if err != nil {
		return plan{}, err
	}
	idx, err := openDB(ctx, indexerDB, false)
	if err != nil {
		return plan{}, err
	}
	defer idx.Close()
	lineage := func(appID, cert string) ([]string, error) { return lineageOf(ctx, idx, appID, cert) }
	pl, err := buildPlan(old, lineage, time.Now())
	if err != nil {
		return plan{}, err
	}
	for _, e := range pending {
		if !strings.HasPrefix(eventAppID(e), devPrefix) {
			pl.Delete = append(pl.Delete, e.ID)
		}
	}
	blobs, err := openDB(ctx, filepath.Join(relayData, "blossom.db"), false)
	if err != nil {
		return plan{}, err
	}
	defer blobs.Close()
	before := len(pl.Dropped)
	err = pl.dropUnserved(func(hash string) (bool, error) {
		var ok bool
		err := blobs.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)`, hash).Scan(&ok)
		return ok, err
	})
	if err != nil {
		return plan{}, err
	}
	unserved := len(pl.Dropped) - before
	if err := writeJSON(filepath.Join(dir, planFile), pl); err != nil {
		return plan{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, droppedFile), []byte(strings.Join(pl.Dropped, "\n")+"\n"), 0o644); err != nil {
		return plan{}, err
	}
	assets := 0
	for _, l := range pl.Listings {
		assets += len(l.Assets)
	}
	t.say("78ce… signed %d events. Plan written to %s:", len(old), filepath.Join(dir, planFile))
	t.say("  re-sign %d apps (%d assets), %d public stacks, a kind 0, and a kind 10067", len(pl.Listings), assets, len(pl.Stacks))
	t.say("  keep %d apps under 78ce…: %v", len(pl.Kept), pl.Kept)
	t.say("  drop %d apps (no complete listing, a 78ce… kind 5, or for %d an asset not in Blossom), listed in %s: %v", len(pl.Dropped), unserved, filepath.Join(dir, droppedFile), pl.Dropped[:min(len(pl.Dropped), 30)])
	t.say("  delete %d events (older releases and assets are in the backup only)", len(pl.Delete))
	return pl, nil
}

// clearOld deletes the plan's events and every snapshot and bundle. epoch-floor stays.
func clearOld(ctx context.Context, t term, relayData string, pl plan) error {
	db, err := openDB(ctx, filepath.Join(relayData, "relay.db"), true)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := dropFTS(ctx, db); err != nil {
		return err
	}
	if err := deleteEvents(ctx, db, pl.Delete); err != nil {
		return err
	}
	for _, name := range []string{catalog.SnapDir(relayData), catalog.BundleDir(relayData)} {
		if err := os.RemoveAll(name); err != nil {
			return err
		}
	}
	t.say("Deleted %d events, snapshots/, and bundles/.", len(pl.Delete))
	return nil
}

const (
	planFile    = "plan.json"
	droppedFile = "dropped.txt"
)

// editIndexerEnv waits until the indexer's SIGN_WITH signs as newKey.
func editIndexerEnv(ctx context.Context, t term, envFile, newKey string) error {
	for {
		if err := t.wait(ctx, "Edit "+envFile+":", "SIGN_WITH=<the new key's bunker URL>"); err != nil {
			return err
		}
		signer, err := signerFor(ctx, envFile)
		if err == nil && signer.PubKey == newKey {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("SIGN_WITH signs as %s, want %s", signer.PubKey, newKey)
		}
		t.say("  ! %v", err)
	}
}

// dryRun rotates a copy of a backup on a throwaway relay with a throwaway key.
func dryRun(ctx context.Context, t term, backup, relayBin string) error {
	if err := requireFTS5(); err != nil {
		return err
	}
	if _, err := readBackup(backup); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "launch-dry-run-")
	if err != nil {
		return err
	}
	t.say("Dry run in %s", tmp)
	data := filepath.Join(tmp, "data")
	if err := copyTree(ctx, filepath.Join(backup, backupRelayData), data); err != nil {
		return err
	}
	pl, err := prepare(ctx, t, data, filepath.Join(backup, backupIndexerData, "indexer.db"), tmp)
	if err != nil {
		return err
	}
	sk := nostr.GeneratePrivateKey()
	signer, err := catalog.ParseSigner(sk)
	if err != nil {
		return err
	}
	lud16 := lud16Of(pl.Profile)
	if lud16 == "" {
		lud16 = "dry-run@example.invalid"
	}
	evs, err := resign(ctx, pl, signer.Sign, signer.PubKey, lud16, time.Now())
	if err != nil {
		return err
	}
	if err := clearOld(ctx, t, data, pl); err != nil {
		return err
	}
	rep, err := storeAll(ctx, t, data, evs)
	rep.print(t)
	if err != nil {
		return err
	}
	env, addr, err := dryRunEnv(filepath.Join(backup, backupRelayEnv), tmp, signer.PubKey, sk)
	if err != nil {
		return err
	}
	ctx, stop, err := startRelay(ctx, relayBin, tmp, env)
	if err != nil {
		return err
	}
	defer stop()
	if err := waitRelay(ctx, t, addr, signer.PubKey); err != nil {
		return fmt.Errorf("%w (log: %s)", err, filepath.Join(tmp, "relay.log"))
	}
	sock := &nostrSocket{path: filepath.Join(data, "relay.sock")}
	defer sock.Close()
	checkErr := checkRotation(ctx, t, sock, signer.PubKey, pl, rep)
	t.say("Relay log: %s", filepath.Join(tmp, "relay.log"))
	if checkErr != nil {
		return checkErr
	}
	t.say("Dry run passed.")
	return nil
}

// dryRunEnv is the backup's relay env with the throwaway key, local ports, and no Bunny or Vertex.
func dryRunEnv(envFile, dir, pubkey, sk string) ([]string, string, error) {
	env, err := readEnv(envFile)
	if err != nil {
		return nil, "", err
	}
	for _, name := range []string{
		"BLOSSOM_BUNNY_CDN_HOSTNAME", "BLOSSOM_BUNNY_ZONE", "BLOSSOM_BUNNY_ZONE_HOSTNAME", "BLOSSOM_BUNNY_PASSWORD",
		"VERTEX_SECRET_KEY", "ANALYTICS_ADDRESS", "READ_ONLY",
	} {
		delete(env, name)
	}
	ports := make([]string, 2)
	for i := range ports {
		addr, err := freeAddr()
		if err != nil {
			return nil, "", err
		}
		ports[i] = addr
	}
	data := filepath.Join(dir, "data")
	maps.Copy(env, map[string]string{
		"DATA_DIR":                data,
		"RELAY_PUBKEY":            pubkey,
		"SIGN_WITH":               sk,
		"CATALOG_BUNDLE_INTERVAL": "0",
		"BLOSSOM_PRUNE_INTERVAL":  "0",
		"RELAY_ADDRESS":           ports[0],
		"BLOSSOM_ADDRESS":         ports[1],
		"RELAY_SOCKET_PATH":       filepath.Join(data, "relay.sock"),
	})
	out := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out, ports[0], nil
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

// startRelay runs `relay run` in dir. The returned context ends when the relay exits.
// stop ends the relay and waits for it.
func startRelay(ctx context.Context, bin, dir string, env []string) (context.Context, func(), error) {
	log, err := os.Create(filepath.Join(dir, "relay.log"))
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "run")
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, nil, fmt.Errorf("start %s: %w", bin, err)
	}
	running, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cancel(fmt.Errorf("relay exited (%v)", cmd.Wait()))
	}()
	return running, func() {
		_ = cmd.Process.Signal(syscall.SIGTERM) // already exited is fine
		<-done
		log.Close()
	}, nil
}
