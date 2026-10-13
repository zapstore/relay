package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zapstore/relay/pkg/events"
)

// artifactFiles are the per-app bundle members, in member order.
var artifactFiles = []string{"about", "icon.webp", "security", "vector"}

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

// BuildBundle writes a client bundle for from→to. Each app in diff.Apps ships
// every artifact the store holds for it. Artifact bytes are the latest files.
func BuildBundle(ctx context.Context, data string, from, to, bundledAt int64, diff Diff, signer Signer) ([]byte, error) {
	raw, err := MarshalDiff(diff)
	if err != nil {
		return nil, err
	}
	var members []member
	if len(raw) > 0 {
		members = append(members, member{Name: "diff.jsonl", Data: raw})
	}
	store, err := openArtifacts(ctx, data)
	if err != nil {
		return nil, err
	}
	if store != nil {
		defer store.Close()
	}
	for _, appID := range diff.Apps {
		if appID == "" || strings.Contains(appID, "/") || strings.Contains(appID, "..") {
			return nil, fmt.Errorf("app id %q", appID)
		}
		for _, name := range artifactFiles {
			body, err := artifactMember(ctx, store, appID, name)
			if err != nil {
				return nil, err
			}
			if len(body) == 0 {
				continue
			}
			members = append(members, member{Name: filepath.ToSlash(filepath.Join(appID, name)), Data: body})
		}
	}
	for _, name := range diff.Avatars {
		if !isAvatarFilename(name) {
			return nil, fmt.Errorf("avatar name %q", name)
		}
		body, err := avatarMember(ctx, store, name)
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			continue
		}
		members = append(members, member{Name: name, Data: body})
	}
	return pack(ctx, members, from, to, bundledAt, &signer)
}
