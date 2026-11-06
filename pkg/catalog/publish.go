package catalog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
// A new snapshot deletes every bundle that does not end at it, and, when keep is above
// zero, the snapshots before epoch n+1−keep with the bundles that start at them.
func CreateBundle(ctx context.Context, data string, next State, signer Signer, bundledAt int64, enrich Enricher, keep int64) (int64, error) {
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
	if next.Artifacts, err = artifactHashes(ctx, data, next); err != nil {
		return 0, err
	}
	if err := finishDiff(ctx, data, &diff, prev, next); err != nil {
		return 0, err
	}
	if latest > 0 && len(diff.Events) == 0 && len(diff.AppDeletes) == 0 && len(diff.Coords) == 0 && len(diff.Apps) == 0 && len(diff.Avatars) == 0 {
		writeBlock("unchanged")
		return latest, nil
	}
	nextN := latest + 1
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
	if err := WriteSnap(snap, next.Events(), next.Notices, next.Artifacts); err != nil {
		return 0, err
	}
	at := time.Unix(bundledAt, 0)
	if err := os.Chtimes(snap, at, at); err != nil {
		return 0, err
	}
	var keepFrom int64
	if keep > 0 {
		keepFrom = nextN - keep
	}
	if err := pruneSnaps(data, keepFrom); err != nil {
		writeBlock("prune old snapshots " + err.Error())
	}
	if err := pruneBundles(data, nextN, keepFrom); err != nil {
		writeBlock("clear old bundles " + err.Error())
	}
	return nextN, nil
}

// ErrFullBundle means the from→to delta is not built: snapshot from is gone, or the
// delta ships more than the allowed share of the catalog. 0→to is served instead.
var ErrFullBundle = errors.New("delta is served as the full bundle")

// EnsureBundle returns the from→to bundle, building it first when it is not cached.
// It never falls back to 0→to for size.
func EnsureBundle(ctx context.Context, data string, from, to int64, signer Signer, notices func(context.Context) (map[string]string, error)) ([]byte, error) {
	if err := WriteBundle(ctx, data, from, to, 0, signer, notices); err != nil {
		return nil, err
	}
	return os.ReadFile(BundlePath(data, from, to))
}

// WriteBundle builds the from→to bundle from the two snapshots unless it is already cached.
// The ban lines are the notices snapshot to was written with. notices is read only for
// a snapshot written before snapshots carried them. Nil means none.
// maxShare above zero is the largest share of the catalog's apps and avatars a delta
// may ship. A larger delta writes a marker instead of the bundle and returns ErrFullBundle,
// as does a from whose snapshot is gone.
func WriteBundle(ctx context.Context, data string, from, to int64, maxShare float64, signer Signer, notices func(context.Context) (map[string]string, error)) error {
	path := BundlePath(data, from, to)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	var prev State
	if from > 0 {
		if _, err := os.Stat(fullMarkerPath(data, from, to)); err == nil {
			return ErrFullBundle
		} else if !os.IsNotExist(err) {
			return err
		}
		st, err := ReadSnap(SnapPath(data, from))
		if errors.Is(err, fs.ErrNotExist) {
			return ErrFullBundle
		}
		if err != nil {
			return err
		}
		prev = st
	}
	next, err := ReadSnap(SnapPath(data, to))
	if err != nil {
		return err
	}
	at, err := snapTime(SnapPath(data, to))
	if err != nil {
		return err
	}
	if next.Notices == nil && notices != nil {
		if next.Notices, err = notices(ctx); err != nil {
			return fmt.Errorf("ban notices: %w", err)
		}
	}
	diff := Compare(prev, next)
	if from == 0 {
		diff.Apps = appIDs(next)
	}
	if err := finishDiff(ctx, data, &diff, prev, next); err != nil {
		return err
	}
	if from > 0 && maxShare > 0 && deltaShare(diff, next) > maxShare {
		if err := writeAtomic(fullMarkerPath(data, from, to), nil); err != nil {
			return err
		}
		return ErrFullBundle
	}
	body, err := BuildBundle(ctx, data, from, to, at.Unix(), diff, signer)
	if err != nil {
		return err
	}
	return writeAtomic(path, body)
}

// deltaShare is the share of the catalog's apps and profiles whose files the diff ships.
// Those files are most of a bundle's size.
func deltaShare(diff Diff, next State) float64 {
	total := len(next.Listings) + len(next.Profiles)
	if total == 0 {
		return 0
	}
	return float64(len(diff.Apps)+len(diff.Avatars)) / float64(total)
}

// finishDiff sets the parts of a diff that read the artifact store: the apps whose
// artifacts match the listing's current APK, the changed avatars, and the ban notices.
// An app whose artifact hash moved since prev ships even when its listing did not.
func finishDiff(ctx context.Context, data string, diff *Diff, prev, next State) error {
	for id, hash := range next.Artifacts {
		if prev.Artifacts[id] != hash && !slices.Contains(diff.Apps, id) {
			diff.Apps = append(diff.Apps, id)
		}
	}
	slices.Sort(diff.Apps)
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

// artifactHashes reads the artifact hash of every listing whose files were
// built for its current APK.
func artifactHashes(ctx context.Context, data string, st State) (map[string]string, error) {
	db, err := openArtifacts(ctx, data)
	if err != nil || db == nil {
		return map[string]string{}, err
	}
	defer db.Close()
	files, err := db.FilesByApp(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, listing := range st.Listings {
		_, hash := apkOf(listing)
		hash = strings.ToLower(strings.TrimSpace(hash))
		f, ok := files[listing.AppID]
		if hash == "" || !ok || strings.ToLower(strings.TrimSpace(f.APK)) != hash {
			continue
		}
		out[listing.AppID] = f.Hash
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
