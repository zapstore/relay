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

	"github.com/caarlos0/env/v11"
	_ "github.com/joho/godotenv/autoload"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/indexing"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
)

type Config struct {
	Sys       SystemConfig
	Limiter   rate.Config
	Analytics analytics.Config
	Indexing  indexing.Config
	Relay     relay.Config
	Blossom   blossom.Config
}

type SystemConfig struct {
	Dir      string   `env:"SYSTEM_DIRECTORY_PATH"`
	LogLevel LogLevel `env:"SYSTEM_LOG_LEVEL"`
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
		Dir:      "", // empty string for the current directory
		LogLevel: LogInfo,
	}
}

func (c SystemConfig) Validate() error {
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
		"\tDirectory Path: %s\n"+
		"\tLog Level: %s\n",
		dir,
		c.LogLevel)
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
	return config, nil
}

func New() Config {
	return Config{
		Sys:       NewSystemConfig(),
		Limiter:   rate.NewConfig(),
		Analytics: analytics.NewConfig(),
		Indexing:  indexing.NewConfig(),
		Relay:     relay.NewConfig(),
		Blossom:   blossom.NewConfig(),
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
	if err := c.Indexing.Validate(); err != nil {
		return fmt.Errorf("indexing: %w", err)
	}
	if err := c.Relay.Validate(); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	if err := c.Blossom.Validate(); err != nil {
		return fmt.Errorf("blossom: %w", err)
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
	b.WriteString(c.Indexing.String())
	b.WriteByte('\n')
	b.WriteString(c.Relay.String())
	b.WriteByte('\n')
	b.WriteString(c.Blossom.String())
	return b.String()
}
