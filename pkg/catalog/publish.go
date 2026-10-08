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
// Identical state writes neither.
// filter keeps diff entries whose app id contains it. It does not add apps that were not already in the diff.
// matchAPK includes an app's artifact files only when its cache apk line equals the listing asset hash.
// enrich runs for the apps in the diff before artifacts are selected. A nil enricher skips that.
// An enricher error is logged and the snapshot is still written.
func CreateBundle(ctx context.Context, data string, next State, signer Signer, bundledAt int64, filter string, matchAPK bool, enrich Enricher) (int64, error) {
	if err := os.MkdirAll(SnapDir(data), 0o755); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(BundleDir(data), 0o755); err != nil {
		return 0, err
	}
	latest, err := LatestSnap(data)
	if err != nil {
		return 0, err
	}
	var prev State
	if latest > 0 {
		prevEvents, err := ReadSnapEvents(SnapPath(data, latest))
		if err != nil {
			return 0, err
		}
		prev = Resolve(prevEvents, "")
	}
	diff := Compare(prev, next)
	diff.Notices = next.Notices
	if latest == 0 {
		diff.Apps = appIDs(next)
		diff.Avatars, err = pictureAvatars(ctx, data, nil, next.Profiles)
	} else {
		diff.Avatars, err = pictureAvatars(ctx, data, prev.Profiles, next.Profiles)
	}
	if err != nil {
		return 0, err
	}
	if filter != "" {
		diff = onlyMatching(diff, next, filter)
	}
	if enrich != nil && len(diff.Apps) > 0 {
		ids := slices.Clone(diff.Apps)
		if err := enrich(ctx, ids); err != nil {
			writeBlock("enrich " + err.Error())
		}
	}
	if matchAPK {
		matched, err := appsMatchingAPK(ctx, data, next, diff.Apps)
		if err != nil {
			return 0, err
		}
		diff.Apps = matched
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
	events := next.Events()
	if err := WriteSnap(SnapPath(data, nextN), events); err != nil {
		return 0, err
	}
	if err := writeAtomic(BundlePath(data, latest, nextN), body); err != nil {
		return 0, err
	}
	return nextN, nil
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

// EnsureBundle returns the cached from→to bundle, building it when absent.
func EnsureBundle(ctx context.Context, data string, from, to int64, signer Signer, bundledAt int64) ([]byte, error) {
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
	nextEvents, err := ReadSnapEvents(SnapPath(data, to))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(SnapPath(data, to))
	at := time.Unix(bundledAt, 0)
	if err == nil {
		at = info.ModTime()
	}
	next := ResolveAt(nextEvents, "", at)
	diff := Compare(prev, next)
	if from == 0 {
		diff.Apps = appIDs(next)
		diff.Avatars, err = pictureAvatars(ctx, data, nil, next.Profiles)
	} else {
		diff.Avatars, err = pictureAvatars(ctx, data, prev.Profiles, next.Profiles)
	}
	if err != nil {
		return nil, err
	}
	body, err := BuildBundle(ctx, data, from, to, bundledAt, diff, signer)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, body); err != nil {
		return nil, err
	}
	return body, nil
}
