package search

import "fmt"

// Config holds Typesense connection settings.
// All fields are read from environment variables via caarlos0/env.
type Config struct {
	// Enabled controls whether Typesense search is active.
	// When false the engine is not started and all search falls through to FTS5.
	// Default: false.
	Enabled bool `env:"TYPESENSE_ENABLED"`

	// URL is the base URL of the Typesense server.
	// Default: "http://localhost:8108".
	URL string `env:"TYPESENSE_URL"`

	// APIKey is the Typesense admin API key.
	// Required when Enabled is true.
	APIKey string `env:"TYPESENSE_API_KEY"`
}

func NewConfig() Config {
	return Config{
		Enabled: false,
		URL:     "http://localhost:8108",
	}
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.URL == "" {
		return fmt.Errorf("TYPESENSE_URL must be set when TYPESENSE_ENABLED=true")
	}
	if c.APIKey == "" {
		return fmt.Errorf("TYPESENSE_API_KEY must be set when TYPESENSE_ENABLED=true")
	}
	return nil
}

func (c Config) String() string {
	if !c.Enabled {
		return "Search:\n\tEnabled: false\n"
	}
	return fmt.Sprintf("Search:\n\tEnabled: true\n\tURL: %s\n", c.URL)
}
