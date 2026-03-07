package blossom

import (
	"fmt"
	"time"

	"github.com/zapstore/relay/pkg/blossom/bunny"
)

type Config struct {
	// Hostname is the hostname of the blossom server, used to validate authorization
	// events and for the "url" field in blob descriptors.
	Hostname string `env:"BLOSSOM_HOSTNAME"`

	// Port is the port the blossom server will listen on. Default is "3335".
	Port string `env:"BLOSSOM_PORT"`

	// AllowedContentTypes is a list of content types that are allowed to be uploaded to the blossom server.
	// Default is "application/vnd.android.package-archive" and common image types.
	AllowedMedia []string `env:"BLOSSOM_ALLOWED_MEDIA"`

	// The no-progress timeout for streaming uploads. Default is 30 seconds.
	StallTimeout time.Duration `env:"BLOSSOM_STALL_TIMEOUT"`

	Bunny bunny.Config
}

func NewConfig() Config {
	return Config{
		Port: "3335",
		AllowedMedia: []string{
		"application/vnd.android.package-archive",
		"application/x-executable",
		"application/x-mach-binary",
		"image/jpeg",
			"image/png",
			"image/webp",
			"image/gif",
			"image/heic",
			"image/heif",
			"image/svg+xml",
		},
		StallTimeout: 30 * time.Second,
		Bunny:        bunny.NewConfig(),
	}
}

func (c Config) Validate() error {
	if c.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	if c.Port == "" {
		return fmt.Errorf("port is required")
	}
	if c.StallTimeout < 5*time.Second {
		return fmt.Errorf("stall timeout must be greater than 5s to function reliably")
	}

	for _, mime := range c.AllowedMedia {
		if mime == "" {
			return fmt.Errorf("allowed media type is empty")
		}
	}

	if err := c.Bunny.Validate(); err != nil {
		return fmt.Errorf("bunny: %w", err)
	}
	return nil
}

func (c Config) String() string {
	return fmt.Sprintf("Blossom:\n"+
		"\tHostname: %s\n"+
		"\tPort: %s\n"+
		"\tAllowed Media: %v\n"+
		"\tStall Timeout: %v\n"+
		c.Bunny.String(), c.Hostname, c.Port, c.AllowedMedia, c.StallTimeout)
}
