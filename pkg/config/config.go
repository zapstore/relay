// The package config is responsible for loading package specific configs from the
// environment variables, and validating them.
//
// Packages requiring configs should expose:
// - A Config struct with the package specific config parameters.
// - A NewConfig() function to create a new Config with default parameters.
// - A Validate() method to validate the config.
// - A String() method to return a string representation of the config.
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	_ "github.com/joho/godotenv/autoload"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/control"
	"github.com/zapstore/relay/pkg/dashboard"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	"github.com/zapstore/relay/pkg/vertex"
)

// Version is the version of the relay, set at build time.
var Version = "dev"

type Config struct {
	Sys       SystemConfig
	Limiter   rate.Config
	Analytics analytics.Config
	Relay     relay.Config
	Blossom   blossom.Config
	Dashboard dashboard.Config
	Control   control.Config
	Catalog   CatalogConfig
	Vertex    vertex.Config
}

// CatalogConfig is signing and the bundle interval.
// GET /v1/bundle is served on the relay listener.
type CatalogConfig struct {
	// SignWith is the catalog signing key: 1, 64-char hex, nsec, or a bunker URL.
	// Its public key must equal RELAY_PUBKEY. A bunker URL is the hotbox signer.
	SignWith string `env:"SIGN_WITH"`

	// Interval is how often a new snapshot is considered. Default is 5 minutes.
	// Zero disables the tick. `relay bundle` writes one snapshot anyway.
	// `relay bundle <app-id>` limits that snapshot to listings whose id contains the app id.
	Interval time.Duration `env:"BUNDLE_INTERVAL"`
}

func NewCatalogConfig() CatalogConfig {
	return CatalogConfig{Interval: 5 * time.Minute}
}

func (c CatalogConfig) Validate() error {
	if strings.TrimSpace(c.SignWith) == "" {
		return fmt.Errorf("SIGN_WITH is empty")
	}
	if c.Interval < 0 {
		return fmt.Errorf("interval must be zero or greater")
	}
	return nil
}

func (c CatalogConfig) String() string {
	sign := "set"
	if strings.TrimSpace(c.SignWith) == "" {
		sign = "empty"
	}
	return fmt.Sprintf("Catalog:\n"+
		"\tSIGN_WITH: %s\n"+
		"\tInterval: %s",
		sign, c.Interval)
}

type SystemConfig struct {
	Version  string   // no env variable, it's set at build time
	Dir      string   `env:"SYSTEM_DIRECTORY_PATH"`
	LogLevel LogLevel `env:"SYSTEM_LOG_LEVEL"`

	// ReadOnly rejects events, Blossom uploads, relay.sock blacklist and whitelist
	// changes, and dashboard blacklist and whitelist changes. Bundles are still signed with SIGN_WITH.
	// The bundle tick still enriches.
	ReadOnly bool `env:"READ_ONLY"`
}

// LogLevel represents the log level for the system.
// It maps string names to [slog.Level] values.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

func (l LogLevel) IsValid() bool {
	switch l {
	case LogDebug, LogInfo, LogWarn, LogError:
		return true
	default:
		return false
	}
}

func NewSystemConfig() SystemConfig {
	return SystemConfig{
		Version:  Version,
		Dir:      "", // empty string for the current directory
		LogLevel: LogInfo,
	}
}

func (c SystemConfig) Validate() error {
	if c.Version == "" {
		return fmt.Errorf("version is not set")
	}
	if !c.LogLevel.IsValid() {
		return fmt.Errorf("invalid log level: %s", c.LogLevel)
	}
	return nil
}

func (c SystemConfig) String() string {
	dir := c.Dir
	if dir == "" {
		dir = "[current directory]"
	}

	return fmt.Sprintf("System:\n"+
		"\tVersion: %s\n"+
		"\tDirectory Path: %s\n"+
		"\tLog Level: %s\n"+
		"\tRead Only: %t\n",
		c.Version,
		dir,
		c.LogLevel,
		c.ReadOnly)
}

// LogOptions returns the [slog.HandlerOptions] for the log level of this config.
func (c SystemConfig) LogOptions() *slog.HandlerOptions {
	switch c.LogLevel {
	case LogDebug:
		return &slog.HandlerOptions{Level: slog.LevelDebug}
	case LogInfo:
		return &slog.HandlerOptions{Level: slog.LevelInfo}
	case LogWarn:
		return &slog.HandlerOptions{Level: slog.LevelWarn}
	case LogError:
		return &slog.HandlerOptions{Level: slog.LevelError}
	default:
		return &slog.HandlerOptions{Level: slog.LevelInfo}
	}
}

// Load creates a new [Config] with default parameters, that get overwritten by env variables when specified.
// It returns an error if the config is invalid.
func Load() (Config, error) {
	config := New()
	if err := env.Parse(&config); err != nil {
		return Config{}, fmt.Errorf("failed to load config: %w", err)
	}
	pk, err := relay.PubkeyHex(config.Relay.Info.Pubkey)
	if err != nil {
		return Config{}, fmt.Errorf("RELAY_PUBKEY: %w", err)
	}
	config.Relay.Info.Pubkey = pk
	return config, nil
}

func New() Config {
	return Config{
		Sys:       NewSystemConfig(),
		Limiter:   rate.NewConfig(),
		Analytics: analytics.NewConfig(),
		Relay:     relay.NewConfig(),
		Blossom:   blossom.NewConfig(),
		Dashboard: dashboard.NewConfig(),
		Control:   control.NewConfig(),
		Catalog:   NewCatalogConfig(),
		Vertex:    vertex.NewConfig(),
	}
}

func (c Config) Validate() error {
	if err := c.Sys.Validate(); err != nil {
		return fmt.Errorf("system: %w", err)
	}
	if err := c.Limiter.Validate(); err != nil {
		return fmt.Errorf("rate: %w", err)
	}
	if err := c.Analytics.Validate(); err != nil {
		return fmt.Errorf("analytics: %w", err)
	}
	if err := c.Relay.Validate(); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	if err := c.Blossom.Validate(); err != nil {
		return fmt.Errorf("blossom: %w", err)
	}
	if err := c.Dashboard.Validate(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	if err := c.Control.Validate(); err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if err := c.Catalog.Validate(); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if err := c.Vertex.Validate(); err != nil {
		return fmt.Errorf("vertex: %w", err)
	}
	return nil
}

func (c Config) String() string {
	var b strings.Builder
	b.WriteString(c.Sys.String())
	b.WriteByte('\n')
	b.WriteString(c.Limiter.String())
	b.WriteByte('\n')
	b.WriteString(c.Analytics.String())
	b.WriteByte('\n')
	b.WriteString(c.Relay.String())
	b.WriteByte('\n')
	b.WriteString(c.Blossom.String())
	b.WriteByte('\n')
	b.WriteString(c.Dashboard.String())
	b.WriteByte('\n')
	b.WriteString(c.Control.String())
	b.WriteByte('\n')
	b.WriteString(c.Catalog.String())
	b.WriteByte('\n')
	b.WriteString(c.Vertex.String())
	return b.String()
}
