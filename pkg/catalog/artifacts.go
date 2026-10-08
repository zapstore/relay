package catalog

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/zapstore/steroid/artifacts"
)

func openArtifacts(ctx context.Context, data string) (*artifacts.DB, error) {
	db, err := artifacts.OpenRead(ctx, artifacts.Path(data))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return db, err
}

func artifactMember(ctx context.Context, db *artifacts.DB, appID, name string) ([]byte, error) {
	if db == nil {
		return nil, nil
	}
	app, err := db.Load(ctx, appID)
	if err != nil || !app.Present {
		return nil, err
	}
	switch name {
	case "about":
		return []byte(app.About), nil
	case "security":
		return []byte(app.Security), nil
	case "vector":
		return app.Vector, nil
	case "icon.webp":
		return app.Icon, nil
	default:
		return nil, nil
	}
}

func avatarMember(ctx context.Context, db *artifacts.DB, name string) ([]byte, error) {
	if db == nil {
		return nil, nil
	}
	pubkey, ok := strings.CutSuffix(name, ".webp")
	if !ok {
		return nil, nil
	}
	avatar, ok, err := db.Avatar(ctx, pubkey)
	if err != nil || !ok {
		return nil, err
	}
	return avatar.WebP, nil
}
