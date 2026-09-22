package bunny

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/caarlos0/env/v11"
	_ "github.com/joho/godotenv/autoload"
)

// Live tests (Test*Live) need Bunny credentials in the environment:
// BUNNY_STORAGE_ZONE_NAME, BUNNY_STORAGE_ZONE_HOSTNAME,
// BUNNY_STORAGE_ZONE_PASSWORD, BUNNY_CDN_HOSTNAME.
// They skip when those are unset. Validation tests run without Bunny.

var (
	config = NewConfig()
	ctx    = context.Background()
)

func init() {
	if err := env.Parse(&config); err != nil {
		panic(fmt.Errorf("failed to parse config: %w", err))
	}
}

func requireBunny(t *testing.T) {
	t.Helper()
	if err := config.Validate(); err != nil || !config.Configured() {
		t.Skip("Bunny is unset")
	}
}

func TestUpload(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		data   io.Reader
		sha256 string
		err    error
	}{
		{
			name: "empty data",
			path: "/tests/test.txt",
			data: nil,
			err:  ErrEmptyData,
		},
		{
			name: "invalid path (empty)",
			path: "",
			data: bytes.NewReader([]byte("This is a test")),
			err:  ErrEmptyPath,
		},
		{
			name:   "invalid sha256",
			path:   "/tests/test.txt",
			data:   bytes.NewReader([]byte("This is a test")),
			sha256: "invalid",
			err:    ErrInvalidChecksum,
		},
	}

	client := Client{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := client.Upload(ctx, test.data, test.path, test.sha256)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected error %v, got %v", test.err, err)
			}
		})
	}
}

func TestUploadLive(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		data   io.Reader
		sha256 string
		err    error
	}{
		{
			name: "valid test (no checksum)",
			path: "/tests/test.txt",
			data: bytes.NewReader([]byte("This is a test")),
		},
		{
			name:   "valid test (with checksum)",
			path:   "/tests/test_with_checksum.txt",
			data:   bytes.NewReader([]byte("This is a test with checksum")),
			sha256: "3323af1d54db3c1c940f90486d1816e9592636125f21b1b29ac927e7a9262ac9",
		},
	}

	requireBunny(t)
	client := NewClient(config)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			err := client.Upload(ctx, test.data, test.path, test.sha256)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected error %v, got %v", test.err, err)
			}
		})
	}
}

func TestDownload(t *testing.T) {
	_, err := (Client{}).Download(ctx, "")
	if !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("expected error %v, got %v", ErrEmptyPath, err)
	}
}

func TestDownloadLive(t *testing.T) {
	tests := []struct {
		name string
		path string
		err  error
	}{
		{
			name: "file does not exists",
			path: "/tests/file_does_not_exist.txt",
			err:  ErrFileNotFound,
		},
		{
			name: "file exists",
			path: "/tests/file_exists.txt",
		},
	}

	requireBunny(t)
	client := NewClient(config)

	expected := []byte("This is a test")
	payload := bytes.NewReader(expected)

	if err := client.Upload(ctx, payload, "/tests/file_exists.txt", ""); err != nil {
		t.Fatalf("failed to upload file: %v", err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := client.Download(ctx, test.path)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected error %v, got %v", test.err, err)
			}

			if err == nil {
				defer data.Close()
				actual, err := io.ReadAll(data)
				if err != nil {
					t.Fatalf("failed to read data: %v", err)
				}
				if !bytes.Equal(actual, expected) {
					t.Fatalf("expected %v, got %v", expected, actual)
				}
			}
		})
	}
}

func TestCheck(t *testing.T) {
	_, _, err := (Client{}).Check(ctx, "")
	if !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("expected error %v, got %v", ErrEmptyPath, err)
	}
}

func TestCheckLive(t *testing.T) {
	tests := []struct {
		name string
		path string
		mime string
		size int64
		err  error
	}{
		{
			name: "file does not exists",
			path: "/tests/file_does_not_exist.txt",
			err:  ErrFileNotFound,
		},
		{
			name: "file exists",
			path: "/tests/file_exists.txt",
			mime: "text/plain",
			size: 14,
		},
	}

	requireBunny(t)
	client := NewClient(config)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mime, size, err := client.Check(ctx, test.path)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected error %v, got %v", test.err, err)
			}

			if mime != test.mime {
				t.Fatalf("expected mime %v, got %v", test.mime, mime)
			}
			if size != test.size {
				t.Fatalf("expected size %v, got %v", test.size, size)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	err := (Client{}).Delete(ctx, "")
	if !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("expected error %v, got %v", ErrEmptyPath, err)
	}
}

func TestDeleteLive(t *testing.T) {
	tests := []struct {
		name string
		path string
		err  error
	}{
		{
			name: "valid delete (file exists)",
			path: "/tests/test.txt",
		},
		{
			name: "valid delete (file does not exist)",
			path: "/tests/test.txt",
		},
	}

	requireBunny(t)
	client := NewClient(config)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := client.Delete(ctx, test.path)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected error %v, got %v", test.err, err)
			}
		})
	}
}

func TestValidateHostname(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		isValid  bool
	}{
		{
			name:     "empty hostname",
			hostname: "",
			isValid:  false,
		},
		{
			name:     "hostname with https scheme",
			hostname: "https://example.com",
			isValid:  false,
		},
		{
			name:     "hostname with http scheme",
			hostname: "http://example.com",
			isValid:  false,
		},
		{
			name:     "hostname with ftp scheme",
			hostname: "ftp://example.com",
			isValid:  false,
		},
		{
			name:     "hsotname with / prefix",
			hostname: "/example.com",
			isValid:  false,
		},
		{
			name:     "hsotname with / suffix	",
			hostname: "example.com/",
			isValid:  false,
		},
		{
			name:     "valid hostname",
			hostname: "example.com",
			isValid:  true,
		},
		{
			name:     "valid hostname with subdomain",
			hostname: "storage.bunnycdn.com",
			isValid:  true,
		},
		{
			name:     "valid hostname with port",
			hostname: "example.com:8080",
			isValid:  true,
		},
		{
			name:     "invalid hostname with path",
			hostname: "example.com/path/to/resource",
			isValid:  false,
		},
		{
			name:     "invalid hostname with query",
			hostname: "example.com?x=1",
			isValid:  false,
		},
		{
			name:     "invalid hostname with fragment",
			hostname: "example.com#frag",
			isValid:  false,
		},
		{
			name:     "invalid hostname with subdomain and path",
			hostname: "cdn.example.com/bucket",
			isValid:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateHostname(test.hostname)
			if (err == nil) != test.isValid {
				t.Errorf("ValidateHostname(%q) error = %v, isValid %v", test.hostname, err, test.isValid)
			}
		})
	}
}
