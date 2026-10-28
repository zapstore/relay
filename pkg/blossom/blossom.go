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
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/blossy"
	"github.com/zapstore/relay/pkg/blossom/bunny"
	"github.com/zapstore/relay/pkg/blossom/store"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
)

const (
	apkType             = "application/vnd.android.package-archive"
	maxAPKBytes   int64 = 500_000_000
	maxImageBytes int64 = 500_000
)

var errTooLarge = errors.New("upload exceeds the size limit")

var (
	ErrClientGone  = &blossom.Error{Code: 499, Reason: "client request was cancelled"}
	ErrNotFound    = blossom.ErrNotFound("blob not found")
	ErrInternal    = blossom.ErrInternal("internal error, please contact the Zapstore team.")
	ErrNotAllowed  = blossom.ErrForbidden("authenticated pubkey is not allowed. Visit https://zapstore.dev/docs/publish for more information.")
	ErrRateLimited = blossom.ErrTooMany("rate-limited: slow down chief")
)

const profileExt = "profile.webp"

type Hash = blossom.Hash

// DB is a pointer to the store.T struct, representing the database connection.
type DB = *store.T

// NewDB creates a new DB instance by opening a SQLite3 database at the given path.
func NewDB(path string) (DB, error) {
	return store.New(path)
}

// T represents the blossoms server and all its dependencies.
type T struct {
	server *blossy.Server
	config Config

	limiter rate.Limiter
	pol     *policy.Store
	blobs   Bytes
	store   *store.T
	relay   Relay
}

// moveEvery is how often promoted blobs are moved from disk to Bunny.
const moveEvery = 10 * time.Second

// Relay is an interface that represents the subset of the relay functionalities needed by the blossoms server.
type Relay interface {
	// ResolveAssetURL looks up a download URL for a kind 3063 asset by its SHA-256 hash.
	// Flutter 1.1.3 asks for it with ?redirect when the APK is not on this relay.
	ResolveAssetURL(ctx context.Context, hash blossom.Hash) (url string, err error)

	// NotifyUpload notifies the relay that an upload has been completed.
	NotifyUpload(hash blossom.Hash, mime string) error
}

func Setup(
	config Config,
	limiter rate.Limiter,
	pol *policy.Store,
	gate *policy.Gate,
	store *store.T,
	relay Relay,
	readOnly bool,
) (*T, error) {

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

	if readOnly {
		server.Reject.Upload.Append(readOnlyUpload)
	}
	server.Reject.Upload.Append(
		RateUploadIP(limiter),
		MissingAuth(),
		MissingHints(),
		MediaNotAllowed(config.AllowedMedia),
		NotAllowed(pol),
		NotAdmitted(gate),
	)

	if config.Dir == "" {
		return nil, fmt.Errorf("blob directory is required: it holds the buffer zone")
	}
	if err := os.MkdirAll(config.Dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create blob directory: %w", err)
	}

	blossom := T{
		server:  server,
		config:  config,
		limiter: limiter,
		pol:     pol,
		blobs:   NewBytes(config, store),
		store:   store,
		relay:   relay,
	}

	server.On.Check = blossom.check
	server.On.Download = blossom.download
	server.On.Upload = blossom.upload
	return &blossom, nil
}

func readOnlyUpload(blossy.Request, blossy.UploadHints) *blossom.Error {
	return blossom.ErrForbidden("relay is read-only")
}

// StartAndServe starts the blossom server, listens to the provided address and handles http requests.
// It’s a blocking operation, that stops only when the context gets cancelled.
func (b *T) StartAndServe(ctx context.Context, addr string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if !b.local() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.blobs.runMover(ctx, moveEvery)
		}()
	}
	err := b.server.StartAndServe(ctx, addr)
	cancel()
	wg.Wait()
	return err
}

func (b *T) local() bool {
	return !b.config.Bunny.Configured()
}

func (b *T) check(r blossy.Request, hash blossom.Hash, ext string) (blossy.MetaDelivery, *blossom.Error) {
	if ext == profileExt {
		if !b.local() {
			return blossy.Redirect(b.profileURL(hash, r.Raw().URL.RawQuery), http.StatusTemporaryRedirect), nil
		}
		info, err := os.Stat(filepath.Join(b.config.Dir, bunny.ProfilePath(hash.Hex())))
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		if err != nil {
			slog.Error("blossom: failed to stat local profile", "error", err, "hash", hash)
			return nil, ErrInternal
		}
		return blossy.Found("image/webp", info.Size()), nil
	}

	// We can check the local store for the blob metadata instead of redirecting to Bunny.
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	meta, err := b.store.Query(ctx, hash)
	if errors.Is(err, context.Canceled) {
		return nil, ErrClientGone
	}
	if errors.Is(err, store.ErrBlobNotFound) || meta.State == store.Pending {
		return nil, ErrNotFound
	}
	if err != nil {
		slog.Error("blossom: failed to query blob metadata", "error", err, "hash", hash)
		return nil, ErrInternal
	}
	return blossy.Found(meta.Type, meta.Size), nil
}

func (b *T) download(r blossy.Request, hash blossom.Hash, ext string) (blossy.BlobDelivery, *blossom.Error) {
	if ext == profileExt {
		if !b.local() {
			return blossy.Redirect(b.profileURL(hash, r.Raw().URL.RawQuery), http.StatusTemporaryRedirect), nil
		}
		f, err := b.blobs.Files.open(bunny.ProfilePath(hash.Hex()))
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		if err != nil {
			slog.Error("blossom: failed to open local profile", "error", err, "hash", hash)
			return nil, ErrInternal
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, ErrInternal
		}
		return blossy.Serve(blossom.BlobFromStream(f, info.Size(), "image/webp")), nil
	}

	// In the Bunny CDN files are defined by their name (hash) and extension (ext).
	// If the extension is not provided, or if it's different (e.g. .jpg instead of .jpeg), Bunny won't find the file.
	// To find the correct extension, we check the store for that hash and use the type to get the extension.
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	meta, err := b.store.Query(ctx, hash)
	if errors.Is(err, context.Canceled) {
		return nil, ErrClientGone
	}
	if err != nil && !errors.Is(err, store.ErrBlobNotFound) {
		slog.Error("blossom: failed to query blob metadata", "error", err, "hash", hash)
		return nil, ErrInternal
	}

	if errors.Is(err, store.ErrBlobNotFound) || meta.State == store.Pending {
		// Flutter 1.1.3 fetches a forge-hosted APK as /<hash>?redirect.
		if !r.Raw().URL.Query().Has("redirect") {
			return nil, ErrNotFound
		}
		ctx, cancel = context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		assetURL, err := b.relay.ResolveAssetURL(ctx, hash)
		if err != nil {
			slog.Error("blossom: failed to resolve asset URL", "hash", hash, "error", err)
			return nil, ErrInternal
		}
		if assetURL == "" {
			return nil, ErrNotFound
		}
		return blossy.Redirect(assetURL, http.StatusTemporaryRedirect), nil
	}

	if b.blobs.onDisk(meta) {
		f, err := b.blobs.Files.open(BlobPath(hash, meta.Type))
		switch {
		case err == nil:
			return blossy.Serve(blossom.BlobFromStream(f, meta.Size, meta.Type)), nil
		case errors.Is(err, os.ErrNotExist) && meta.State == store.Moving:
			// The mover put it on Bunny after the metadata was read.
		case errors.Is(err, os.ErrNotExist):
			return nil, ErrNotFound
		default:
			slog.Error("blossom: failed to open local blob", "error", err, "hash", hash)
			return nil, ErrInternal
		}
	}
	query := r.Raw().URL.Query()
	url := b.blobs.Bunny.CDNURLWithRawQuery(
		BlobPath(hash, meta.Type),
		(url.Values{"class": query["class"]}).Encode(),
	)
	return blossy.Redirect(url, http.StatusTemporaryRedirect), nil
}

func (b *T) profileURL(pubkey blossom.Hash, rawQuery string) string {
	return b.blobs.Bunny.CDNURLWithRawQuery(bunny.ProfilePath(pubkey.Hex()), rawQuery)
}

func (b *T) upload(r blossy.Request, hints blossy.UploadHints, data io.Reader) (blossom.BlobDescriptor, *blossom.Error) {
	if data == nil {
		slog.Error("blossom: received nil body on upload")
		return blossom.BlobDescriptor{}, blossom.ErrBadRequest("body is empty")
	}

	// A blob that is already here is not written again. A pending one waits
	// for its listing from now.
	meta, err := b.store.Query(r.Context(), *hints.Hash)
	if err == nil {
		if blocked := b.rejectBannedFile(r.Context(), meta); blocked != nil {
			return blossom.BlobDescriptor{}, blocked
		}
		if err := b.store.Renew(r.Context(), meta.Hash, time.Now()); err != nil {
			slog.Error("blossom: failed to renew pending blob", "error", err, "hash", meta.Hash)
			return blossom.BlobDescriptor{}, ErrInternal
		}
		return b.localDescriptor(blossom.BlobDescriptor{
			Hash:     meta.Hash,
			Type:     meta.Type,
			Size:     meta.Size,
			Uploaded: meta.CreatedAt.Unix(),
		}), nil
	}
	if errors.Is(err, context.Canceled) {
		return blossom.BlobDescriptor{}, ErrClientGone
	}
	if err != nil && !errors.Is(err, store.ErrBlobNotFound) {
		// internal error
		slog.Error("blossom: failed to query blob metadata", "error", err, "hash", hints.Hash)
		return blossom.BlobDescriptor{}, ErrInternal
	}

	name := BlobPath(*hints.Hash, hints.Type)

	max, ok := maxUploadBytes(hints.Type)
	if !ok {
		return blossom.BlobDescriptor{}, blossom.ErrUnsupportedMedia("content type is not allowed")
	}
	limited := &byteLimit{r: data, max: max}
	reader := newStallReader(r.Context(), limited, b.config.StallTimeout)
	defer reader.Stop()

	size, err := b.blobs.Files.put(name, reader, (*hints.Hash)[:])
	if errors.Is(err, errTooLarge) || errors.Is(limited.err, errTooLarge) || errors.Is(reader.Err(), errTooLarge) {
		return blossom.BlobDescriptor{}, blossom.ErrBadRequest("upload exceeds the size limit")
	}
	if errors.Is(err, errChecksumMismatch) {
		b.limiter.Penalize(r.IP().Group(), 200)
		return blossom.BlobDescriptor{}, blossom.ErrBadRequest("checksum mismatch")
	}
	if rErr := reader.Err(); rErr != nil {
		return blossom.BlobDescriptor{}, &blossom.Error{Code: 499, Reason: rErr.Error()}
	}
	if err != nil {
		slog.Error("blossom: failed to write blob", "error", err, "name", name)
		return blossom.BlobDescriptor{}, ErrInternal
	}

	meta = store.BlobMeta{
		Hash:       *hints.Hash,
		Type:       hints.Type,
		Size:       size,
		CreatedAt:  time.Now().UTC(),
		AuthPubkey: r.Pubkey(),
		State:      store.Pending,
	}
	if blocked := b.rejectBannedFile(reader.Context(), meta); blocked != nil {
		return blossom.BlobDescriptor{}, blocked
	}

	// punish the client for providing bad hints.
	if hints.Size < size {
		cost := 100.0
		b.limiter.Penalize(r.IP().Group(), cost)
	}

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer saveCancel()

	_, err = b.store.Save(saveCtx, meta)
	if err != nil {
		slog.Error("blossom: failed to save blob metadata", "error", err, "hash", hints.Hash)
		return blossom.BlobDescriptor{}, ErrInternal
	}

	if err := b.relay.NotifyUpload(meta.Hash, meta.Type); err != nil {
		slog.Error("blossom: failed to notify relay of upload", "error", err, "hash", meta.Hash)
	}
	return b.localDescriptor(blossom.BlobDescriptor{
		Hash:     *hints.Hash,
		Type:     hints.Type,
		Size:     size,
		Uploaded: meta.CreatedAt.Unix(),
	}), nil
}

// localDescriptor names the blob on this process's listen address.
// Blossom otherwise advertises https://hostname, which is the CDN name and
// is not the HTTP port a local server is bound to.
func (b *T) localDescriptor(desc blossom.BlobDescriptor) blossom.BlobDescriptor {
	if !b.local() || b.config.Address == "" || desc.URL != "" {
		return desc
	}
	desc.URL = "http://" + b.config.Address + "/" + desc.Hash.Hex() + "." + blossom.ExtFromType(desc.Type)
	return desc
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

func NotAllowed(pol *policy.Store) func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		if pol == nil || !r.IsAuthed() {
			return nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		pub, normErr := policy.Normalize(policy.KindNostr, r.Pubkey())
		if normErr != nil {
			return nil
		}
		banned, checkErr := pol.Blacklisted(ctx, pub)
		if checkErr != nil {
			slog.Error("policy: failed to check pubkey", "err", checkErr)
			return ErrInternal
		}
		if banned {
			return ErrNotAllowed
		}
		return nil
	}
}

// NotAdmitted runs the publisher gate for uploads. A key that cannot publish
// a new app cannot upload, unless it is the active C1 owner or delegate of a
// certificate on an app the relay already lists.
func NotAdmitted(gate *policy.Gate) func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
	return func(r blossy.Request, hints blossy.UploadHints) *blossom.Error {
		if gate == nil || !r.IsAuthed() {
			return nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		err := gate.AdmitUpload(ctx, r.Pubkey())
		if errors.Is(err, policy.ErrNotAdmitted) {
			return ErrNotAllowed
		}
		if err != nil {
			slog.Error("policy: gate failed", "err", err)
			return ErrInternal
		}
		return nil
	}
}

// rejectBannedFile runs after the APK bytes are stored. A known hash deletes
// that object and rejects the upload. The uploader is not blacklisted.
func (b *T) rejectBannedFile(ctx context.Context, meta store.BlobMeta) *blossom.Error {
	if b.pol == nil {
		return nil
	}
	hash := meta.Hash.Hex()
	_, banned, err := b.pol.HashReason(ctx, hash)
	if err != nil {
		slog.Error("policy: failed to check blob", "err", err, "hash", hash)
		return ErrInternal
	}
	if !banned {
		return nil
	}
	if rmErr := b.blobs.remove(ctx, meta); rmErr != nil {
		slog.Error("blossom: failed to remove banned blob", "err", rmErr, "hash", hash)
	}
	return blossom.ErrForbidden("file is not allowed")
}

func maxUploadBytes(mime string) (int64, bool) {
	switch mime {
	case apkType:
		return maxAPKBytes, true
	case "image/jpeg", "image/png", "image/webp", "image/gif", "image/heic", "image/heif":
		return maxImageBytes, true
	default:
		return 0, false
	}
}

// byteLimit counts bytes from r and returns errTooLarge once max is passed.
type byteLimit struct {
	r   io.Reader
	max int64
	n   int64
	err error
}

func (b *byteLimit) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.n > b.max {
		b.err = errTooLarge
		return 0, errTooLarge
	}
	if b.n == b.max {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			b.n += int64(n)
			b.err = errTooLarge
			return 0, errTooLarge
		}
		if err == nil {
			err = io.EOF
		}
		b.err = err
		return 0, err
	}
	if int64(len(p)) > b.max-b.n {
		p = p[:b.max-b.n]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	return n, err
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

		ip := r.IP().Group()
		if !limiter.Allow(ip, cost) {
			slog.Debug("blossom: rejecting upload", "ip", ip)
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
			slog.Debug("blossom: rejecting download", "ip", ip)
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
			slog.Debug("blossom: rejecting check", "ip", ip)
			return ErrRateLimited
		}
		return nil
	}
}
