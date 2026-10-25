package catalog

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// CreateBundle writes snapshot n+1 and the bundle from n to n+1.
// A snapshot is the full catalog at one epoch. A bundle is the client diff between two epochs.
// Identical state writes neither. bundledAt is the epoch time: the manifest's created_at and
// the snapshot's modification time, so a later rebuild of the same span is identical.
// enrich runs for the apps in the diff before artifacts are selected. A nil enricher skips that.
// An enricher error is logged and the snapshot is still written.
func CreateBundle(ctx context.Context, data string, next State, signer Signer, bundledAt int64, enrich Enricher) (int64, error) {
	if err := os.MkdirAll(SnapDir(data), 0o755); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(BundleDir(data), 0o755); err != nil {
		return 0, err
	}
	unlock, err := lockSeal(data)
	if err != nil {
		return 0, err
	}
	defer unlock()
	latest, err := LatestSnap(data)
	if err != nil {
		return 0, err
	}
	var prev State
	if latest > 0 {
		if prev, err = ReadSnap(SnapPath(data, latest)); err != nil {
			return 0, err
		}
	}
	diff := Compare(prev, next)
	if latest == 0 {
		diff.Apps = appIDs(next)
	}
	if enrich != nil && len(diff.Apps) > 0 {
		if err := enrich(ctx, slices.Clone(diff.Apps)); err != nil {
			writeBlock("enrich " + err.Error())
		}
	}
	if err := finishDiff(ctx, data, &diff, prev, next); err != nil {
		return 0, err
	}
	if latest > 0 && len(diff.Events) == 0 && len(diff.AppDeletes) == 0 && len(diff.Coords) == 0 && len(diff.Apps) == 0 && len(diff.Avatars) == 0 {
		writeBlock("unchanged")
		return latest, nil
	}
	floor, err := ReadFloor(data)
	if err != nil {
		return 0, err
	}
	// After a rebuild the first snapshot continues above every epoch already served,
	// so no phone ever sees an epoch number reused for a different catalog.
	nextN := max(latest, floor) + 1
	writeBlock(fmt.Sprintf("snapshot %d, bundle %d-%d", nextN, latest, nextN))
	body, err := BuildBundle(ctx, data, latest, nextN, bundledAt, diff, signer)
	if err != nil {
		return 0, err
	}
	// The bundle goes first: the snapshot makes nextN the served epoch.
	if err := writeAtomic(BundlePath(data, latest, nextN), body); err != nil {
		return 0, err
	}
	snap := SnapPath(data, nextN)
	if err := WriteSnap(snap, next.Events(), next.Notices); err != nil {
		return 0, err
	}
	at := time.Unix(bundledAt, 0)
	if err := os.Chtimes(snap, at, at); err != nil {
		return 0, err
	}
	if err := writeFloor(data, nextN); err != nil {
		return 0, err
	}
	return nextN, nil
}

// EnsureBundle returns the cached from→to bundle, building it from the two snapshots when absent.
// The ban lines are the notices snapshot to was written with. notices is read only for
// a snapshot written before snapshots carried them. Nil means none.
func EnsureBundle(ctx context.Context, data string, from, to int64, signer Signer, notices func(context.Context) (map[string]string, error)) ([]byte, error) {
	path := BundlePath(data, from, to)
	if raw, err := os.ReadFile(path); err == nil {
		return raw, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var prev State
	if from > 0 {
		st, err := ReadSnap(SnapPath(data, from))
		if err != nil {
			return nil, err
		}
		prev = st
	}
	next, err := ReadSnap(SnapPath(data, to))
	if err != nil {
		return nil, err
	}
	at, err := snapTime(SnapPath(data, to))
	if err != nil {
		return nil, err
	}
	if next.Notices == nil && notices != nil {
		if next.Notices, err = notices(ctx); err != nil {
			return nil, fmt.Errorf("ban notices: %w", err)
		}
	}
	diff := Compare(prev, next)
	if from == 0 {
		diff.Apps = appIDs(next)
	}
	if err := finishDiff(ctx, data, &diff, prev, next); err != nil {
		return nil, err
	}
	body, err := BuildBundle(ctx, data, from, to, at.Unix(), diff, signer)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, body); err != nil {
		return nil, err
	}
	return body, nil
}

// finishDiff sets the parts of a diff that read the artifact store: the apps whose
// artifacts match the listing's current APK, the changed avatars, and the ban notices.
func finishDiff(ctx context.Context, data string, diff *Diff, prev, next State) error {
	matched, err := appsMatchingAPK(ctx, data, next, diff.Apps)
	if err != nil {
		return err
	}
	diff.Apps = matched
	if diff.Avatars, err = pictureAvatars(ctx, data, prev.Profiles, next.Profiles); err != nil {
		return err
	}
	diff.Notices = next.Notices
	return nil
}

// appsMatchingAPK keeps apps whose cache was produced for the listing's current asset hash.
func appsMatchingAPK(ctx context.Context, data string, st State, ids []string) ([]string, error) {
	db, err := openArtifacts(ctx, data)
	if err != nil || db == nil {
		return nil, err
	}
	defer db.Close()
	hashOf := make(map[string]string, len(st.Listings))
	for _, listing := range st.Listings {
		_, hash := apkOf(listing)
		hashOf[listing.AppID] = strings.ToLower(strings.TrimSpace(hash))
	}
	var out []string
	for _, id := range ids {
		hash := hashOf[id]
		if hash == "" {
			continue
		}
		app, err := db.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		if !app.Present || strings.ToLower(strings.TrimSpace(app.Memo.APK)) != hash {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

func appIDs(st State) []string {
	out := make([]string, len(st.Listings))
	for i, listing := range st.Listings {
		out[i] = listing.AppID
	}
	return out
}

func pictureAvatars(ctx context.Context, data string, prev, next []Profile) ([]string, error) {
	old := map[string]string{}
	for _, profile := range prev {
		old[profile.Pubkey] = profile.Picture
	}
	db, err := openArtifacts(ctx, data)
	if err != nil || db == nil {
		return nil, err
	}
	defer db.Close()
	var out []string
	for _, profile := range next {
		if profile.Picture == "" || old[profile.Pubkey] == profile.Picture {
			continue
		}
		name, err := avatarFilename(profile.Pubkey)
		if err != nil {
			continue
		}
		avatar, ok, err := db.Avatar(ctx, profile.Pubkey)
		if err != nil {
			return nil, err
		}
		if !ok || strings.TrimSpace(avatar.Picture) != profile.Picture {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}
