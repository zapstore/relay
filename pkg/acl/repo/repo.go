package repo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"gopkg.in/yaml.v3"
)

// VerifyResult holds the outcome of a repository verification.
type VerifyResult struct {
	// Matched is true when the zapstore.yaml pubkey matches the event author.
	Matched bool

	// Platform is the detected platform (e.g., "github", "gitlab", "codeberg").
	Platform string

	// Username is the repository owner's username on the platform.
	Username string

	// RepoURL is the canonical repository URL used for verification.
	RepoURL string
}

// Verifier fetches zapstore.yaml from a repository and checks the pubkey.
type Verifier struct {
	http   *http.Client
	config Config
}

// NewVerifier creates a new Verifier with the given configuration.
func NewVerifier(c Config) *Verifier {
	return &Verifier{
		http:   &http.Client{Timeout: c.Timeout},
		config: c,
	}
}

// Verify checks whether the zapstore.yaml in the given repository URL
// contains a pubkey that matches eventPubkey (hex).
// Returns a VerifyResult and an error. A non-nil error means verification
// could not be completed (network failure, file not found, etc.).
func (v *Verifier) Verify(ctx context.Context, repoURL, eventPubkey string) (VerifyResult, error) {
	platform, rawURL, username, err := resolveRawURL(repoURL)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("unsupported or unparseable repo URL %q: %w", repoURL, err)
	}

	pubkeyHex, err := v.fetchPubkey(ctx, rawURL, platform)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("failed to fetch zapstore.yaml from %s: %w", repoURL, err)
	}

	return VerifyResult{
		Matched:  pubkeyHex == eventPubkey,
		Platform: platform,
		Username: username,
		RepoURL:  repoURL,
	}, nil
}

// zapstoreYAML holds the relevant fields from zapstore.yaml.
type zapstoreYAML struct {
	Pubkey string `yaml:"pubkey"`
}

// fetchPubkey downloads zapstore.yaml from rawURL and returns the pubkey as hex.
func (v *Verifier) fetchPubkey(ctx context.Context, rawURL, platform string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}

	if platform == "github" && v.config.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+v.config.GitHubToken)
	}

	resp, err := v.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("zapstore.yaml not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var z zapstoreYAML
	if err := yaml.NewDecoder(resp.Body).Decode(&z); err != nil {
		return "", fmt.Errorf("failed to decode zapstore.yaml: %w", err)
	}

	return normalizePubkey(z.Pubkey)
}

// normalizePubkey converts npub or hex pubkey to hex.
func normalizePubkey(pk string) (string, error) {
	if nostr.IsValidPublicKey(pk) {
		return pk, nil
	}
	if strings.HasPrefix(pk, "npub1") {
		_, data, err := nip19.Decode(pk)
		if err != nil {
			return "", fmt.Errorf("invalid npub: %w", err)
		}
		hex, ok := data.(string)
		if !ok {
			return "", fmt.Errorf("unexpected npub data type")
		}
		return hex, nil
	}
	return "", fmt.Errorf("pubkey %q is neither valid hex nor npub", pk)
}

// resolveRawURL maps a repository URL to the raw content URL for zapstore.yaml.
// Returns platform name, raw content URL, owner username, and any error.
func resolveRawURL(repoURL string) (platform, rawURL, username string, err error) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return "", "", "", fmt.Errorf("failed to parse URL: %w", err)
	}

	host := strings.ToLower(u.Host)
	parts := pathParts(u.Path)
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("URL path must contain owner and repo")
	}
	owner, repo := parts[0], parts[1]

	switch {
	case host == "github.com":
		rawURL = fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/HEAD/zapstore.yaml", owner, repo)
		return "github", rawURL, owner, nil

	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		encoded := url.PathEscape(owner + "/" + repo)
		rawURL = fmt.Sprintf("https://gitlab.com/%s/%s/-/raw/HEAD/zapstore.yaml", owner, repo)
		_ = encoded
		return "gitlab", rawURL, owner, nil

	case host == "codeberg.org":
		rawURL = fmt.Sprintf("https://codeberg.org/%s/%s/raw/branch/main/zapstore.yaml", owner, repo)
		return "codeberg", rawURL, owner, nil

	default:
		// Generic Gitea/Forgejo instance
		rawURL = fmt.Sprintf("%s://%s/%s/%s/raw/branch/main/zapstore.yaml", u.Scheme, u.Host, owner, repo)
		return "gitea", rawURL, owner, nil
	}
}

// pathParts splits a URL path into non-empty segments.
func pathParts(p string) []string {
	var parts []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}
