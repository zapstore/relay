package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/events"
)

// deployV2 is window 1: relay v2 on oldKey, with no snapshot, enrichment, or prune.
func deployV2(ctx context.Context, t term) error {
	s, err := loadSite(ctx, t)
	if err != nil {
		return err
	}
	t.say("\nWindow 1: relay v2 on 78ce….")
	if err := t.confirm("Is Flutter 1.1.3 released?"); err != nil {
		return err
	}
	if err := t.confirm("Are writes disabled on the deployed zapstore.dev webapp?"); err != nil {
		return err
	}
	newBin, err := t.ask("Relay v2 binary to install", "")
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, newBin, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version: %s", newBin, firstLine(out, err))
	}
	t.say("Relay v2 version: %s", firstLine(out, nil))

	p, err := s.paths()
	if err != nil {
		return err
	}
	if err := t.confirm(fmt.Sprintf("Stop %s, %s, and %s now?", s.ProxyUnit, s.IndexerUnit, s.RelayUnit)); err != nil {
		return err
	}
	if err := stopAll(ctx, t, s); err != nil {
		return err
	}
	backup, err := takeBackup(t, s, p, "deploy-v2")
	if err != nil {
		return err
	}
	if err := copyTree(newBin, s.RelayBinary); err != nil {
		return fmt.Errorf("install %s: %w", s.RelayBinary, err)
	}
	t.say("Installed %s", s.RelayBinary)
	if err := dropRelayFTS(ctx, p.RelayData); err != nil {
		return err
	}
	t.say("Dropped apps_fts from relay.db")

	_, signer, err := editRelayEnv(ctx, t, s.RelayEnv, relayEnvWant{key: oldKey},
		"RELAY_PUBKEY stays 78ce…",
		"BUNDLE_INTERVAL=0",
		"PRUNE_INTERVAL=0",
	)
	if err != nil {
		return err
	}
	if err := systemctl(ctx, t, "start", s.RelayUnit); err != nil {
		return err
	}
	if err := waitRelay(ctx, t, p.RelayAddr, oldKey); err != nil {
		return err
	}
	sock := &nostrSocket{path: p.socket()}
	defer sock.Close()
	if err := ensureRelays(ctx, t, sock, signer); err != nil {
		return err
	}
	if err := checkOneApp(ctx, t, sock); err != nil {
		return err
	}
	if err := reenableSelfPublished(ctx, t, p); err != nil {
		return err
	}
	for _, unit := range []string{s.IndexerUnit, s.ProxyUnit} {
		if err := systemctl(ctx, t, "start", unit); err != nil {
			return err
		}
	}
	t.say("\nWindow 1 is done. Bundles and prune stay off until rotation.")
	t.say("Run steroid to fill artifacts.db. Undo with: launch restore %s", backup)
	t.say("The indexer still signs as 78ce…. Re-enabled apps publish at their next poll.")
	return nil
}

// ensureRelays publishes the catalog key's kind 10067 when the relay has none.
func ensureRelays(ctx context.Context, t term, sock *nostrSocket, signer catalog.Signer) error {
	evs, err := sock.query(ctx, nostr.Filter{Kinds: []int{events.KindAppRelays}, Authors: []string{signer.PubKey}, Limit: 1})
	if err != nil {
		return err
	}
	if len(evs) > 0 {
		t.say("The catalog key's kind 10067 is stored.")
		return nil
	}
	if err := t.confirm("The catalog key has no kind 10067. Publish one with r " + relayHint + "?"); err != nil {
		return err
	}
	e := nostr.Event{CreatedAt: nostr.Timestamp(time.Now().Unix()), Kind: events.KindAppRelays, Tags: nostr.Tags{{"r", relayHint}}}
	if err := signer.Sign(ctx, &e); err != nil {
		return fmt.Errorf("sign kind 10067: %w", err)
	}
	ok, msg, err := sock.publish(ctx, e)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("relay rejected the kind 10067: %s", msg)
	}
	t.say("Published the kind 10067.")
	return nil
}

// checkOneApp asks for an app id and checks the relay serves one listing for it.
func checkOneApp(ctx context.Context, t term, sock *nostrSocket) error {
	appID, err := t.ask("An app id to check", "dev.zapstore.app")
	if err != nil {
		return err
	}
	evs, err := sock.query(ctx, nostr.Filter{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": {appID}}, Limit: 10})
	if err != nil {
		return err
	}
	switch len(evs) {
	case 0:
		return fmt.Errorf("the relay serves no kind 32267 for %s", appID)
	case 1:
		t.say("%s resolves to one listing, signed by %s.", appID, evs[0].PubKey)
		return nil
	}
	t.say("  ! %s returns %d kind 32267 events: slot selection is not applied on REQ.", appID, len(evs))
	return t.confirm("Continue anyway?")
}

// reenableSelfPublished re-enables indexer apps that are disabled and have a listing from another key.
func reenableSelfPublished(ctx context.Context, t term, p paths) error {
	relayDB, err := openDB(ctx, p.relayDB(), false)
	if err != nil {
		return err
	}
	defer relayDB.Close()
	developer, err := appIDsSignedByOthers(ctx, relayDB, oldKey)
	if err != nil {
		return err
	}
	indexerDB, err := openDB(ctx, p.indexerDB(), true)
	if err != nil {
		return err
	}
	defer indexerDB.Close()
	disabled, err := disabledApps(ctx, indexerDB)
	if err != nil {
		return err
	}
	var ids []string
	for _, a := range disabled {
		if developer[a.ID] {
			ids = append(ids, a.ID)
			t.say("  %s  (%s)", a.ID, a.Reason)
		}
	}
	if len(ids) == 0 {
		t.say("No disabled indexer app has a self-published listing.")
		return nil
	}
	ok, err := t.yes(fmt.Sprintf("Re-enable these %d apps in the indexer?", len(ids)))
	if err != nil || !ok {
		return err
	}
	if err := enableApps(ctx, indexerDB, ids); err != nil {
		return err
	}
	t.say("Re-enabled %d apps.", len(ids))
	return nil
}
