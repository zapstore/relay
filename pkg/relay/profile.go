package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chai2010/webp"
	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"golang.org/x/image/draw"
)

const (
	profileImageSize    = 256
	profileImageQuality = 75
	profileImageMaxSize = 10 << 20
	profileImageMaxSide = 4096
)

type kind0Profile struct {
	Picture string `json:"picture"`
}

func (r *T) enqueueProfile(pubkey string) {
	select {
	case r.profileJobs <- pubkey:
	default:
		slog.Warn("profile processor queue is full", "pubkey", pubkey)
	}
}

func (r *T) runProfileWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pubkey := <-r.profileJobs:
			if err := r.processProfile(ctx, pubkey); err != nil {
				slog.Warn("profile processing failed", "pubkey", pubkey, "error", err)
			}
		}
	}
}

func (r *T) processProfile(ctx context.Context, pubkey string) error {
	if !nostr.IsValid32ByteHex(pubkey) {
		return fmt.Errorf("invalid profile pubkey %q", pubkey)
	}

	profile, err := r.fetchProfile(ctx, pubkey)
	if err != nil {
		return err
	}

	var data kind0Profile
	if err := json.Unmarshal([]byte(profile.Content), &data); err != nil {
		return fmt.Errorf("invalid kind 0 content: %w", err)
	}
	picture := strings.TrimSpace(data.Picture)

	if picture == "" {
		return nil
	}

	encoded, err := fetchAndEncodeProfile(ctx, picture)
	if err != nil {
		return err
	}

	if err := r.profileUploader.UploadProfile(ctx, pubkey, bytes.NewReader(encoded)); err != nil {
		return err
	}

	slog.Info("profile picture processed", "pubkey", pubkey, "source", picture, "bytes", len(encoded))
	return nil
}

func (r *T) fetchProfile(ctx context.Context, pubkey string) (*nostr.Event, error) {
	var latest *nostr.Event
	var errs []error

	for _, relayURL := range r.config.ProfileRelays {
		queryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		upstream := nostr.NewRelay(queryCtx, relayURL)

		found, err := upstream.QuerySync(queryCtx, nostr.Filter{
			Kinds:   []int{events.KindProfile},
			Authors: []string{pubkey},
			Limit:   1,
		})
		_ = upstream.Close()
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", relayURL, err))
			continue
		}
		for _, event := range found {
			if event.Kind != events.KindProfile || event.PubKey != pubkey {
				continue
			}
			if ok, err := event.CheckSignature(); err != nil || !ok {
				continue
			}
			if latest == nil || event.CreatedAt > latest.CreatedAt {
				latest = event
			}
		}
	}

	if latest == nil {
		if len(errs) == 0 {
			return nil, errors.New("profile not found on configured relays")
		}
		return nil, fmt.Errorf("profile not found on configured relays: %w", errors.Join(errs...))
	}

	saved, err := r.store.Replace(ctx, latest)
	if err != nil {
		return nil, fmt.Errorf("failed to save fetched kind 0: %w", err)
	}
	if saved {
		if err := r.server.Broadcast(latest); err != nil {
			slog.Debug("failed to broadcast fetched kind 0", "event", latest.ID, "error", err)
		}
		return latest, nil
	}

	stored, err := r.store.Query(ctx, nostr.Filter{
		Kinds:   []int{events.KindProfile},
		Authors: []string{pubkey},
		Limit:   1,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to load stored kind 0: %w", err)
	}
	if len(stored) == 0 {
		return nil, errors.New("fetched kind 0 was not saved")
	}
	return &stored[0], nil
}

func fetchAndEncodeProfile(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("profile picture must be an HTTPS URL")
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if err := validatePublicHTTPSURL(req.URL); err != nil {
				return err
			}
			return nil
		},
	}
	if err := validatePublicHTTPSURL(parsed); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("profile picture returned %s", res.Status)
	}
	if res.ContentLength > profileImageMaxSize {
		return nil, errors.New("profile picture exceeds 10 MiB")
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, profileImageMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read profile picture: %w", err)
	}
	if len(raw) > profileImageMaxSize {
		return nil, errors.New("profile picture exceeds 10 MiB")
	}

	var source image.Image
	if strings.HasPrefix(http.DetectContentType(raw), "image/webp") {
		source, err = webp.DecodeRGBA(raw)
	} else {
		source, _, err = image.Decode(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to decode profile picture: %w", err)
	}
	if source.Bounds().Dx() > profileImageMaxSide || source.Bounds().Dy() > profileImageMaxSide {
		return nil, errors.New("profile picture dimensions exceed 4096px")
	}

	resized := squareResize(source, profileImageSize)
	encoded, err := webp.EncodeRGBA(resized, profileImageQuality)
	if err != nil {
		return nil, fmt.Errorf("failed to encode profile picture: %w", err)
	}
	return encoded, nil
}

func squareResize(source image.Image, size int) *image.RGBA {
	bounds := source.Bounds()
	side := bounds.Dx()
	if bounds.Dy() < side {
		side = bounds.Dy()
	}
	x := bounds.Min.X + (bounds.Dx()-side)/2
	y := bounds.Min.Y + (bounds.Dy()-side)/2
	crop := image.Rect(x, y, x+side, y+side)

	target := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(target, target.Bounds(), source, crop, draw.Over, nil)
	return target
}

func validatePublicHTTPSURL(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("profile picture redirects must use HTTPS")
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return fmt.Errorf("failed to resolve profile picture host: %w", err)
	}
	for _, ip := range ips {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("profile picture host resolves to a private address")
		}
	}
	return nil
}
