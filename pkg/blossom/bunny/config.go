package bunny

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	StorageZone StorageZone

	// The hostname of the CDN (e.g. "zapstore.b-cdn.net"). It must not include the scheme.
	CDN string `env:"BUNNY_CDN_HOSTNAME"`

	// The hard ceiling for requests to Bunny. Default is 20 minutes.
	RequestTimeout time.Duration `env:"BUNNY_REQUEST_TIMEOUT"`
}

type StorageZone struct {
	// The username of the storage zone (e.g. "your-username").
	Name string `env:"BUNNY_STORAGE_ZONE_NAME"`

	// The hostname of the primary region of the storage zone (e.g. "storage.bunnycdn.com").
	// It must not include the scheme.
	Hostname string `env:"BUNNY_STORAGE_ZONE_HOSTNAME"`

	// The password of the storage zone (e.g. "your-password").
	Password string `env:"BUNNY_STORAGE_ZONE_PASSWORD"`
}

func NewConfig() Config {
	return Config{
		RequestTimeout: 20 * time.Minute,
	}
}

// set reports whether any Bunny field was provided. Partial configs fail Validate.
func (c Config) set() bool {
	return c.CDN != "" || c.StorageZone.Name != "" || c.StorageZone.Hostname != "" || c.StorageZone.Password != ""
}

// Configured reports whether Bunny is fully set and the relay should use the CDN.
func (c Config) Configured() bool {
	return c.CDN != "" && c.StorageZone.Name != "" && c.StorageZone.Hostname != "" && len(c.StorageZone.Password) >= 8
}

func (c Config) Validate() error {
	if !c.set() {
		return nil
	}
	if c.StorageZone.Name == "" {
		return errors.New("storage zone name must be specified")
	}
	if err := ValidateHostname(c.StorageZone.Hostname); err != nil {
		return fmt.Errorf("storage zone hostname: %w", err)
	}
	if len(c.StorageZone.Password) < 8 {
		return errors.New("storage zone password must be specified and at least 8 characters long")
	}
	if c.RequestTimeout < 20*time.Second {
		return errors.New("request timeout must be greater than 20s to function reliably")
	}

	if err := ValidateHostname(c.CDN); err != nil {
		return fmt.Errorf("CDN hostname: %w", err)
	}
	return nil
}

// ValidateHostname validates that a hostname string is not empty, is a valid hostname,
// and does not contain a scheme (e.g., "example.com" is valid, "https://example.com" is not).
func ValidateHostname(hostname string) error {
	if hostname == "" {
		return errors.New("hostname must not be empty")
	}
	if strings.Contains(hostname, "://") {
		return errors.New("hostname must not include a scheme (e.g., http:// or https://)")
	}
	if strings.HasPrefix(hostname, "/") || strings.HasSuffix(hostname, "/") {
		return errors.New("hostname must not include a leading or trailing slash")
	}
	parsed, err := url.Parse("https://" + hostname)
	if err != nil {
		return fmt.Errorf("invalid hostname: %w", err)
	}
	if parsed.Host == "" {
		return errors.New("hostname must contain a valid host")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("hostname must not include a path, query, or fragment")
	}
	return nil
}

func (c Config) String() string {
	if !c.Configured() {
		return "Bunny:\n\t(unset, local blobs)\n"
	}
	return fmt.Sprintf("Bunny:\n"+
		"\tRequest Timeout: %v\n"+
		"\tCDN Hostname: %s\n"+
		"\tStorageZone:\n"+
		"\t\tName: %s\n"+
		"\t\tHostname: %s\n"+
		"\t\tPassword: %s\n",
		c.RequestTimeout,
		c.CDN,
		c.StorageZone.Name,
		c.StorageZone.Hostname,
		redactSecret(c.StorageZone.Password),
	)
}

func redactSecret(s string) string {
	if len(s) < 8 {
		return "(unset)"
	}
	return s[:4] + "___REDACTED___" + s[len(s)-4:]
}
