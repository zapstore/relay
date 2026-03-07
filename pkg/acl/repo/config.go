// Package repo provides platform-agnostic repository verification for the ACL.
// It fetches zapstore.yaml from a repository and checks that the pubkey matches the event author.
package repo

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Config holds configuration for the repo verifier HTTP client.
type Config struct {
	// Timeout is the maximum time to wait for a response. Default is 10 seconds.
	Timeout time.Duration `env:"REPO_REQUEST_TIMEOUT"`

	// GitHubToken is an optional GitHub API token to increase rate limits.
	GitHubToken string `env:"GITHUB_API_TOKEN"`
}

// NewConfig returns a Config with sensible defaults.
func NewConfig() Config {
	return Config{
		Timeout: 5 * time.Second,
	}
}

// Validate checks that the configuration is valid.
func (c Config) Validate() error {
	if c.Timeout < 2*time.Second {
		return errors.New("timeout must be at least 2 seconds to function reliably")
	}
	if c.GitHubToken == "" {
		slog.Warn("GitHub API token not set, which may cause stricter rate-limiting.")
	}
	return nil
}

// String returns a redacted string representation of the config.
func (c Config) String() string {
	token := "[not set]"
	if c.GitHubToken != "" {
		token = c.GitHubToken[:4] + "___REDACTED___" + c.GitHubToken[len(c.GitHubToken)-4:]
	}
	return fmt.Sprintf("Repo:\n\tTimeout: %s\n\tGitHubToken: %s\n", c.Timeout, token)
}
