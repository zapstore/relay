// The blossom package is responsible for setting up the blossom server.
// It exposes a [Setup] function to create a new relay with the given config.
package blossom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/blossy"
	defender "github.com/zapstore/defender/pkg/client"
	"github.com/zapstore/defender/pkg/models"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom/bunny"
	"github.com/zapstore/relay/pkg/blossom/store"
	"github.com/zapstore/relay/pkg/rate"
)

var (
	ErrNotFound    = blossom.ErrNotFound("blob not found")
	ErrInternal    = blossom.ErrInternal("internal error, please contact the Zapstore team.")
	ErrNotAllowed  = blossom.ErrForbidden("authenticated pubkey is not allowed. Visit https://zapstore.dev/docs/publish for more information.")
	ErrRateLimited = blossom.ErrTooMany("rate-limited: slow down chief")
)

// AssetResolver looks up a download URL for a kind 3063 asset by its SHA-256 hash.
type AssetResolver interface {
	ResolveAssetURL(ctx context.Context, hash string) (url string, found bool, err error)
}

type Hash = blossom.Hash

func Setup(
	config Config,
	limiter rate.Limiter,
	defender defender.T,
	store *store.Store,
	analytics *analytics.Engine,
	resolver AssetResolver,
) (*blossy.Server, error) {

	bunny := bunny.NewClient(config.Bunny)
	server, err := blossy.NewServer(
		blossy.WithHostname(config.Hostname),
		blossy.WithRangeSupport(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to setup blossom server: %w", err)
	}

	server.Reject.Check.Append(
		RateCheckIP(limiter),
	)

	server.Reject.Download.Append(
		RateDownloadIP(limiter),
	)

	server.Reject.Upload.Append(
		RateUploadIP(limiter),
		MissingAuth(),
		MissingHints(),
		MediaNotAllowed(config.AllowedMedia),
		NotAllowed(defender),
	)

	server.On.Check = Check(store, analytics)
	server.On.Download = Download(store, bunny, resolver, analytics)
	server.On.Upload = Upload(store, bunny, limiter, config.StallTimeout, analytics)
	return server, nil
}

func Check(db *store.Store, analytics *analytics.Engine) func(r blossy.Request, hash blossom.Hash, ext string) (blossy.MetaDelivery, *blossom.Error) {
	return func(r blossy.Request, hash blossom.Hash, _ string) (blossy.MetaDelivery, *blossom.Error) {

		// We can check the local store for the blob metadata instead of redirecting to Bunny.
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()

		meta, err := db.Query(ctx, hash)
		if errors.Is(err, store.ErrBlobNotFound) {
			return nil, ErrNotFound
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("blossom: failed to query blob metadata", "error", err, "hash", hash)
			return nil, ErrInternal
		}

		analytics.RecordCheck(r, hash)
		return blossy.Found(meta.Type, meta.Size), nil
	}
}

func Download(db *store.Store, client bunny.Client, resolver AssetResolver, analytics *analytics.Engine) func(r blossy.Request, hash blossom.Hash, _ string) (blossy.BlobDelivery, *blossom.Error) {
	return func(r blossy.Request, hash blossom.Hash, _ string) (blossy.BlobDelivery, *blossom.Error) {

		// In the Bunny CDN files are defined by their name (hash) and extension (ext).
		// If the extension is not provided, or if it's different (e.g. .jpg instead of .jpeg), Bunny won't find the file.
		// To find the correct extension, we check the store for that hash and use the type to get the extension.
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()

		meta, err := db.Query(ctx, hash)
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if err != nil && !errors.Is(err, store.ErrBlobNotFound) {
			slog.Error("blossom: failed to query blob metadata", "error", err, "hash", hash)
			return nil, ErrInternal
		}

		if errors.Is(err, store.ErrBlobNotFound) {
			// blob not found locally; if the client opted in, try the nostr redirect resolver.
			redirect := r.Raw().URL.Query().Has("redirect")
			if !redirect {
				return nil, ErrNotFound
			}

			ctx, cancel = context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()

			assetURL, found, err := resolver.ResolveAssetURL(ctx, hash.Hex())
			if err != nil {
				slog.Error("blossom: failed to resolve asset URL", "hash", hash, "error", err)
				return nil, ErrInternal
			}
			if !found || assetURL == "" {
				return nil, ErrNotFound
			}

			analytics.RecordDownload(r, hash)
			return blossy.Redirect(assetURL, http.StatusTemporaryRedirect), nil
		}

		analytics.RecordDownload(r, hash)
		url := client.CDNURL(BlobPath(hash, meta.Type))
		return blossy.Redirect(url, http.StatusTemporaryRedirect), nil
	}
}

// stallReader resets a timer on every successful Read, enabling stall detection for streaming uploads.
type stallReader struct {
	data    io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.data.Read(p)
	if n > 0 {
		s.timer.Reset(s.timeout)
	}
	return n, err
}

func Upload(
	db *store.Store,
	client bunny.Client,
	limiter rate.Limiter,
	stallTimeout time.Duration,
	analytics *analytics.Engine,
) func(r blossy.Request, hints blossy.UploadHints, data io.Reader) (blossom.BlobDescriptor, *blossom.Error) {
	return func(r blossy.Request, hints blossy.UploadHints, data io.Reader) (blossom.BlobDescriptor, *blossom.Error) {
		if data == nil {
			return blossom.BlobDescriptor{}, blossom.ErrBadRequest("body is empty")
		}

		// To avoid wasting bandwidth and Bunny credits,
		// we check if the blob exists in the store before uploading it.
		meta, err := db.Query(r.Context(), *hints.Hash)
		if err == nil {
			// blob already exists
			return blossom.BlobDescriptor{
				Hash:     meta.Hash,
				Type:     meta.Type,
				Size:     meta.Size,
				Uploaded: meta.CreatedAt.Unix(),
			}, nil
		}
		if errors.Is(err, context.Canceled) {
			return blossom.BlobDescriptor{}, blossom.ErrBadRequest("context canceled")
		}
		if err != nil && !errors.Is(err, store.ErrBlobNotFound) {
			// internal error
			slog.Error("blossom: failed to query blob metadata", "error", err, "hash", hints.Hash)
			return blossom.BlobDescriptor{}, ErrInternal
		}

		// upload to Bunny directly, enforcing the stall timeout to prevent clients from uploading too slowly.
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)

		reader := &stallReader{
			data:    data,
			timeout: stallTimeout,
			timer: time.AfterFunc(stallTimeout, func() {
				cancel(fmt.Errorf("stalled longer than %v", stallTimeout))
			}),
		}
		defer reader.timer.Stop()

		name := BlobPath(*hints.Hash, hints.Type)
		sha256 := hints.Hash.Hex()

		err = client.Upload(ctx, reader, name, sha256)
		if errors.Is(err, bunny.ErrInvalidChecksum) {
			// punish the client for providing a bad hash
			cost := 200.0
			limiter.Penalize(r.IP().Group(), cost)
			return blossom.BlobDescriptor{}, blossom.ErrBadRequest("checksum mismatch")
		}
		if errors.Is(err, context.Canceled) {
			return blossom.BlobDescriptor{}, blossom.ErrBadRequest("context canceled")
		}
		if err != nil {
			slog.Error("blossom: failed to upload blob", "error", err, "name", name, "ctx_error", ctx.Err(), "ctx_cause", context.Cause(ctx))
			return blossom.BlobDescriptor{}, ErrInternal
		}

		// Use a fresh context for the remaining operations to avoid orphaning
		// blobs in Bunny if the client disconnects after the upload completes.
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer saveCancel()

		_, size, err := client.Check(saveCtx, name)
		if err != nil {
			slog.Error("blossom: failed to check blob", "error", err, "name", name)
			return blossom.BlobDescriptor{}, ErrInternal
		}

		// punish the client for providing bad hints.
		if hints.Size < size {
			cost := 100.0
			limiter.Penalize(r.IP().Group(), cost)
		}

		meta = store.BlobMeta{
			Hash:      *hints.Hash,
			Type:      hints.Type,
			Size:      size,
			CreatedAt: time.Now().UTC(),
		}

		_, err = db.Save(saveCtx, meta)
		if err != nil {
			slog.Error("blossom: failed to save blob metadata", "error", err, "hash", hints.Hash)
			return blossom.BlobDescriptor{}, ErrInternal
		}

		analytics.RecordUpload(r, hints)
		return blossom.BlobDescriptor{
			Hash:     *hints.Hash,
			Type:     hints.Type,
			Size:     size,
			Uploaded: meta.CreatedAt.Unix(),
		}, nil
	}
}

// BlobPath returns the path to the blob on the blossom server, based on the hash and mime type.
func BlobPath(hash blossom.Hash, mime string) string {
	return "blobs/" + hash.Hex() + "." + blossom.ExtFromType(mime)
}

func MissingAuth() func(r blossy.Request, _ blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		if !r.IsAuthed() {
			return blossom.ErrUnauthorized("authentication is required")
		}
		return nil
	}
}

func MissingHints() func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		if hints.Hash == nil {
			return blossom.ErrBadRequest("'Content-Digest' header is required")
		}
		if hints.Type == "" {
			return blossom.ErrBadRequest("'Content-Type' header is required")
		}
		if hints.Size == -1 {
			return blossom.ErrBadRequest("'Content-Length' header is required")
		}
		return nil
	}
}

func NotAllowed(defender defender.T) func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		meta := models.BlobMeta{
			Pubkey: r.Pubkey(),
			Hash:   *hints.Hash,
			Type:   hints.Type,
			Size:   hints.Size,
		}

		res, err := defender.CheckBlob(ctx, meta)
		if err != nil {
			slog.Error("defender: failed to check blob", "err", err, "hash", hints.Hash)
			return ErrInternal
		}

		if res.Decision == models.DecisionReject {
			return ErrNotAllowed
		}
		return nil
	}
}

func MediaNotAllowed(allowed []string) func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		if !slices.Contains(allowed, hints.Type) {
			reason := fmt.Sprintf("content type is not in the allowed list: %s", strings.Join(allowed, ", "))
			return blossom.ErrUnsupportedMedia(reason)
		}
		return nil
	}
}

func RateUploadIP(limiter rate.Limiter) func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		// The default cost is 50 tokens to punish clients that don't provide the size.
		// Otherwise, the cost is 1 token per 10 MB.
		cost := 50.0
		if hints.Size > 0 {
			cost = float64(hints.Size) / 10_000_000
		}

		if !limiter.Allow(r.IP().Group(), cost) {
			return ErrRateLimited
		}
		return nil
	}
}

func RateDownloadIP(limiter rate.Limiter) func(r blossy.Request, hash blossom.Hash, ext string) *blossom.Error {
	return func(r blossy.Request, hash blossom.Hash, ext string) *blossom.Error {
		cost := 10.0
		ip := r.IP().Group()

		if !limiter.Allow(ip, cost) {
			return ErrRateLimited
		}
		return nil
	}
}

func RateCheckIP(limiter rate.Limiter) func(r blossy.Request, hash blossom.Hash, ext string) *blossom.Error {
	return func(r blossy.Request, hash blossom.Hash, ext string) *blossom.Error {
		cost := 1.0
		ip := r.IP().Group()

		if !limiter.Allow(ip, cost) {
			return ErrRateLimited
		}
		return nil
	}
}
