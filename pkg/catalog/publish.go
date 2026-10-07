package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	diff.Avatars = pictureAvatars(data, prev.Profiles, next.Profiles)
	if latest == 0 {
		diff.Apps = appIDs(next)
		diff.Avatars = pictureAvatars(data, nil, next.Profiles)
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
		diff.Apps = appsMatchingAPK(data, next, diff.Apps)
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
func appsMatchingAPK(data string, st State, ids []string) []string {
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
		m, ok := readMemo(AppDir(data, id))
		if !ok || strings.ToLower(strings.TrimSpace(m.APK)) != hash {
			continue
		}
		out = append(out, id)
	}
	return out
}

func appIDs(st State) []string {
	out := make([]string, len(st.Listings))
	for i, listing := range st.Listings {
		out[i] = listing.AppID
	}
	return out
}

func pictureAvatars(data string, prev, next []Profile) []string {
	old := map[string]string{}
	for _, profile := range prev {
		old[profile.Pubkey] = profile.Picture
	}
	var out []string
	for _, profile := range next {
		if profile.Picture == "" || old[profile.Pubkey] == profile.Picture {
			continue
		}
		name, err := avatarFilename(profile.Pubkey)
		if err != nil {
			continue
		}
		side, err := os.ReadFile(filepath.Join(ArtifactDir(data), profile.Pubkey+".url"))
		if err != nil || strings.TrimSpace(string(side)) != profile.Picture {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
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
	next := Resolve(nextEvents, "")
	diff := Compare(prev, next)
	if from == 0 {
		diff.Apps = appIDs(next)
		diff.Avatars = pictureAvatars(data, nil, next.Profiles)
	} else {
		diff.Avatars = pictureAvatars(data, prev.Profiles, next.Profiles)
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
