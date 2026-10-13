package blossom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/zapstore/relay/pkg/blossom/bunny"
	"github.com/zapstore/relay/pkg/blossom/store"
)

// Bytes is the blossom metadata database plus the blob body, on disk or Bunny.
// An upload stays on disk in the buffer zone until its listing is promoted.
type Bytes struct {
	DB    *store.T
	Files Files
	Bunny CDN
	Local bool
}

// CDN is the part of [bunny.Client] that blobs use.
type CDN interface {
	Upload(ctx context.Context, data io.Reader, path string, sha256 string) error
	Check(ctx context.Context, path string) (mime string, size int64, err error)
	Download(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	CDNURLWithRawQuery(path string, rawQuery string) string
}

// NewBytes opens blobs for a blossom config. Dir must already be set.
func NewBytes(cfg Config, db *store.T) Bytes {
	return Bytes{
		DB:    db,
		Files: Files{Dir: cfg.Dir},
		Bunny: bunny.NewClient(cfg.Bunny),
		Local: !cfg.Bunny.Configured(),
	}
}

// Has reports whether the blob is in the buffer zone or served.
func (b Bytes) Has(ctx context.Context, hash Hash) (bool, error) {
	if b.DB == nil {
		return false, errors.New("blossom store is not set")
	}
	return b.DB.Has(ctx, hash)
}

// Open returns the blob body. The caller closes it. A body on disk is an
// [*os.File].
func (b Bytes) Open(ctx context.Context, hash Hash) (io.ReadCloser, error) {
	if b.DB == nil {
		return nil, errors.New("blossom store is not set")
	}
	meta, err := b.DB.Query(ctx, hash)
	if err != nil {
		return nil, err
	}
	path := BlobPath(hash, meta.Type)
	if b.onDisk(meta) {
		return b.Files.open(path)
	}
	return b.Bunny.Download(ctx, path)
}

// Promote serves a blob from the buffer zone. With Bunny set, the blob is
// served from disk until [Bytes.Move] puts it on Bunny. A blob that is not
// pending is left alone.
func (b Bytes) Promote(ctx context.Context, hash Hash) error {
	to := store.Moving
	if b.Local {
		to = store.Served
	}
	_, err := b.DB.SetState(ctx, hash, store.Pending, to)
	return err
}

// Expire deletes blobs that were still pending before the cutoff.
func (b Bytes) Expire(ctx context.Context, before time.Time) (int, error) {
	expired, err := b.DB.InState(ctx, store.Pending, before)
	if err != nil {
		return 0, err
	}
	var n int
	for _, meta := range expired {
		if err := b.remove(ctx, meta); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Move puts every promoted blob that is still on disk on Bunny, then deletes
// the disk copy. It does nothing when Bunny is unset.
func (b Bytes) Move(ctx context.Context) error {
	if b.Local {
		return nil
	}
	moving, err := b.DB.InState(ctx, store.Moving, time.Time{})
	if err != nil {
		return err
	}
	var errs []error
	for _, meta := range moving {
		if err := b.move(ctx, meta); err != nil {
			errs = append(errs, fmt.Errorf("move %s: %w", meta.Hash, err))
		}
	}
	return errors.Join(errs...)
}

func (b Bytes) move(ctx context.Context, meta store.BlobMeta) error {
	path := BlobPath(meta.Hash, meta.Type)
	f, err := b.Files.open(path)
	if err != nil {
		return err
	}
	err = b.Bunny.Upload(ctx, f, path, meta.Hash.Hex())
	f.Close()
	if err != nil {
		return err
	}
	_, size, err := b.Bunny.Check(ctx, path)
	if err != nil {
		return err
	}
	if size != meta.Size {
		return fmt.Errorf("bunny has %d bytes, want %d", size, meta.Size)
	}
	if _, err := b.DB.SetState(ctx, meta.Hash, store.Moving, store.Served); err != nil {
		return err
	}
	return b.Files.remove(path)
}

// remove deletes the blob body and its metadata.
func (b Bytes) remove(ctx context.Context, meta store.BlobMeta) error {
	path := BlobPath(meta.Hash, meta.Type)
	if err := b.Files.remove(path); err != nil {
		return err
	}
	if !b.Local && meta.State != store.Pending {
		if err := b.Bunny.Delete(ctx, path); err != nil {
			return err
		}
	}
	return b.DB.Delete(ctx, meta.Hash)
}

// onDisk reports whether the body is in the blob directory.
func (b Bytes) onDisk(meta store.BlobMeta) bool {
	return b.Local || meta.State != store.Served
}

// runMover moves promoted blobs to Bunny every interval until ctx ends.
func (b Bytes) runMover(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := b.Move(ctx); err != nil && ctx.Err() == nil {
				slog.Error("blossom: failed to move blobs to Bunny", "error", err)
			}
		}
	}
}
