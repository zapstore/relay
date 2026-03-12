// Package repourl identifies and normalises repository URLs from free-text search terms.
// A repository URL is any URL whose path has exactly two non-empty segments: /:user/:repo.
// This shape is common across GitHub, GitLab, Codeberg, etc.
package repourl

import (
	"net/url"
	"strings"
)

// RepoURL holds the canonical form of a repository URL and its parsed components.
type RepoURL struct {
	// Canonical is "https://<host>/<user>/<repo>" with no trailing slash or .git.
	Canonical string
	Host      string
	User      string
	Repo      string
}

// Parse parses rawSearch as a repository URL.
// It accepts URLs with or without a scheme, and ignores any trailing path segments,
// query parameters, or fragments beyond the /:user/:repo prefix.
// Returns (RepoURL, true) when the input resolves to a two-segment repo path;
// returns (RepoURL{}, false) for plain-text searches or malformed input.
func Parse(rawSearch string) (RepoURL, bool) {
	s := strings.TrimSpace(rawSearch)
	if s == "" {
		return RepoURL{}, false
	}

	// Ensure there is a scheme so net/url parses the host correctly.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return RepoURL{}, false
	}

	// Split path into non-empty segments.
	segments := make([]string, 0, 2)
	for _, seg := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if seg != "" {
			segments = append(segments, seg)
		}
	}

	// Require exactly two segments: /:user/:repo
	if len(segments) != 2 {
		return RepoURL{}, false
	}

	user := segments[0]
	repo := strings.TrimSuffix(segments[1], ".git")

	canonical := "https://" + u.Hostname() + "/" + user + "/" + repo

	return RepoURL{
		Canonical: canonical,
		Host:      u.Hostname(),
		User:      user,
		Repo:      repo,
	}, true
}
