package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func apkOf(listing Listing) (url, hash string) {
	var fallback events.Asset
	var have bool
	for _, asset := range listing.Assets {
		parsed, err := events.ParseAsset(&asset)
		if err != nil {
			continue
		}
		if parsed.URL == "" && parsed.Hash == "" {
			continue
		}
		for _, platform := range parsed.Platforms {
			if platform == preferredABI && parsed.URL != "" && parsed.Hash != "" {
				return parsed.URL, parsed.Hash
			}
		}
		if !have {
			fallback = parsed
			have = true
		}
	}
	if !have {
		return "", ""
	}
	return fallback.URL, fallback.Hash
}

// BuildBundle writes a client bundle for from→to. Artifact bytes are the latest files.
// about, vector, and icon.webp ship only when the diff has that app's kind 32267 event.
// security ships only when the diff has that app's kind 3063 event.
func BuildBundle(ctx context.Context, data string, from, to, bundledAt int64, diff Diff, signer Signer) ([]byte, error) {
	raw, err := MarshalDiff(diff)
	if err != nil {
		return nil, err
	}
	var members []member
	if len(raw) > 0 {
		members = append(members, member{Name: "diff.jsonl", Data: raw})
	}
	listing, asset := enrichmentApps(diff.Events)
	ids := enrichmentIDs(listing, asset, diff.Apps)
	for _, appID := range ids {
		if appID == "" || strings.Contains(appID, "/") || strings.Contains(appID, "..") {
			return nil, fmt.Errorf("app id %q", appID)
		}
		dir := AppDir(data, appID)
		for _, name := range enrichmentFiles(appID, listing, asset) {
			body, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			if len(body) == 0 {
				continue
			}
			memberName := filepath.ToSlash(filepath.Join(appID, name))
			members = append(members, member{Name: memberName, Data: body})
		}
	}
	for _, name := range diff.Avatars {
		if !isAvatarFilename(name) {
			return nil, fmt.Errorf("avatar name %q", name)
		}
		body, err := os.ReadFile(filepath.Join(ArtifactDir(data), name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		members = append(members, member{Name: name, Data: body})
	}
	return pack(ctx, members, from, to, bundledAt, &signer)
}

func enrichmentApps(list []nostr.Event) (listing, asset map[string]struct{}) {
	listing = map[string]struct{}{}
	asset = map[string]struct{}{}
	for _, event := range list {
		switch event.Kind {
		case events.KindApp:
			if id, ok := events.Find(event.Tags, "d"); ok && id != "" {
				listing[id] = struct{}{}
			}
		case events.KindAsset:
			if id, ok := events.Find(event.Tags, "i"); ok && id != "" {
				asset[id] = struct{}{}
			}
		}
	}
	return listing, asset
}

func enrichmentIDs(listing, asset map[string]struct{}, allowed []string) []string {
	okID := map[string]struct{}{}
	for _, id := range allowed {
		okID[id] = struct{}{}
	}
	seen := map[string]struct{}{}
	var ids []string
	for id := range listing {
		if _, ok := okID[id]; !ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range asset {
		if _, ok := okID[id]; !ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func enrichmentFiles(appID string, listing, asset map[string]struct{}) []string {
	var names []string
	if _, ok := listing[appID]; ok {
		names = append(names, "about", "vector", "icon.webp")
	}
	if _, ok := asset[appID]; ok {
		names = append(names, "security")
	}
	return names
}
