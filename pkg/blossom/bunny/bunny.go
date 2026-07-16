// The bunny package is responsible for interacting with the Bunny Storage Zone and CDN.
// It exposes a [NewClient] function to create a new client with the given config.
package bunny

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pippellia-btc/blossom"
)

var (
	ErrEmptyData        = errors.New("empty data")
	ErrEmptyPath        = errors.New("empty path")
	ErrInvalidChecksum  = errors.New("invalid sha256 checksum")
	ErrChecksumMismatch = errors.New("checksum mismatch")
	ErrFileNotFound     = errors.New("file not found")
)

type Client struct {
	http   http.Client
	config Config
}

// NewClient returns a client from the provided [Config], which is assumed to have been validated.
func NewClient(c Config) Client {
	return Client{
		http: http.Client{
			Timeout: c.RequestTimeout,
			Transport: &http.Transport{
				// Force HTTP/1.1 to avoid HTTP/2 GOAWAY errors, which are currently
				// impossible to handle gracefully because the request body is not replayable,
				// as it comes from the request body sent to our blossom server.
				ForceAttemptHTTP2: false,
				TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
			},
		},
		config: c,
	}
}

// StorageURL returns the request URL for the provided path on the storage zone.
func (c Client) StorageURL(path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	return fmt.Sprintf("https://%s/%s/%s",
		c.config.StorageZone.Hostname,
		c.config.StorageZone.Name,
		strings.TrimPrefix(path, "/"),
	)
}

// CDNURL returns the request URL for the provided path on the CDN.
func (c Client) CDNURL(path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	return fmt.Sprintf("https://%s/%s", c.config.CDN, path)
}

// CDNURLWithRawQuery returns the request URL for the provided path and raw query on the CDN.
func (c Client) CDNURLWithRawQuery(path string, rawQuery string) string {
	cdnURL := c.CDNURL(path)
	if rawQuery == "" {
		return cdnURL
	}
	return cdnURL + "?" + rawQuery
}

// Download the file at the specified path.
// Returns the reader for the file, or an error if the file does not exist.
// The caller is responsible for closing the reader.
func (c Client) Download(ctx context.Context, path string) (io.ReadCloser, error) {
	if path == "" {
		return nil, fmt.Errorf("bunny: failed to download: %w", ErrEmptyPath)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.StorageURL(path), nil,
	)
	if err != nil {
		return nil, fmt.Errorf("bunny: failed to create request: %w", err)
	}

	c.setHeaders(req)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bunny: failed to download: %w", err)
	}

	if res.StatusCode == http.StatusOK {
		return res.Body, nil
	}

	res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("bunny: failed to download: %w", ErrFileNotFound)
	}
	return nil, fmt.Errorf("bunny: failed to download: status %s", res.Status)
}

// Check returns the metadata of the file at the specified path on the CDN.
func (c Client) Check(ctx context.Context, path string) (mime string, size int64, err error) {
	if path == "" {
		return "", 0, fmt.Errorf("bunny: failed to check: %w", ErrEmptyPath)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodHead, c.CDNURL(path), nil,
	)

	if err != nil {
		return "", 0, fmt.Errorf("bunny: failed to check: failed to create request: %w", err)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("bunny: failed to check: %w", err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
		mime = res.Header.Get("Content-Type")
		size, err = strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("bunny: failed to check: failed to parse content length: %w", err)
		}
		return mime, size, nil

	case http.StatusNotFound:
		return "", 0, fmt.Errorf("bunny: failed to check: %w", ErrFileNotFound)

	default:
		return "", 0, fmt.Errorf("bunny: failed to check: status %s", res.Status)
	}
}

// Upload the data to the Bunny storage zone with the specified path.
//
// If the hex-encoded sha256 is not empty (not ""), it will be used to set the "Checksum" header.
// If the sha256(data) != sha256, Bunny will reject the upload and [ErrChecksumMismatch] will be returned.
func (c Client) Upload(ctx context.Context, data io.Reader, path string, sha256 string) error {
	return c.upload(ctx, data, path, sha256, "")
}

func (c Client) upload(ctx context.Context, data io.Reader, path string, sha256 string, contentType string) error {
	if data == nil {
		return fmt.Errorf("bunny: failed to upload: %w", ErrEmptyData)
	}
	if path == "" {
		return fmt.Errorf("bunny: failed to upload: %w", ErrEmptyPath)
	}
	if sha256 != "" {
		if err := blossom.ValidateHash(sha256); err != nil {
			return fmt.Errorf("bunny: failed to upload: %w: %w", ErrInvalidChecksum, err)
		}
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPut, c.StorageURL(path), data,
	)
	if err != nil {
		return fmt.Errorf("bunny: failed to create request: %w", err)
	}

	c.setHeaders(req)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if sha256 != "" {
		req.Header.Add("Checksum", strings.ToUpper(sha256))
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bunny: failed to upload: %w", err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusCreated:
		return nil

	case http.StatusBadRequest:
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "Checksum") {
			return fmt.Errorf("bunny: failed to upload: %w", ErrChecksumMismatch)
		}
		return fmt.Errorf("bunny: failed to upload: status %s: body %s", res.Status, string(body))

	default:
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("bunny: failed to upload: status %s: body %s", res.Status, string(body))
	}
}

// UploadProfile stores a processed profile picture at its stable CDN path.
func (c Client) UploadProfile(ctx context.Context, pubkey string, data io.Reader) error {
	if pubkey == "" {
		return fmt.Errorf("bunny: failed to upload profile: %w", ErrEmptyPath)
	}
	path := "p/" + pubkey + ".webp"
	if err := c.upload(ctx, data, path, "", "image/webp"); err != nil {
		return fmt.Errorf("bunny: failed to upload profile: %w", err)
	}
	return nil
}

// Delete the file at the specified path.
// Returns nil if the file was deleted successfully, or if the file did not exist.
func (c Client) Delete(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("bunny: failed to delete: %w", ErrEmptyPath)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodDelete, c.StorageURL(path), nil,
	)
	if err != nil {
		return fmt.Errorf("bunny: failed to create request: %w", err)
	}

	c.setHeaders(req)

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bunny: failed to delete: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound ||
		res.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("bunny: failed to delete: %s", res.Status)
}

// setHeaders sets the common headers for the request.
func (c Client) setHeaders(r *http.Request) {
	r.Header.Add("accept", "application/json")
	r.Header.Add("AccessKey", c.config.StorageZone.Password)
}
