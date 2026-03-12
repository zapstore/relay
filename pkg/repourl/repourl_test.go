package repourl

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantCanonical string
		wantHost      string
		wantUser      string
		wantRepo      string
		wantMatch     bool
	}{
		{
			name:          "canonical https URL",
			input:         "https://github.com/user/repo",
			wantCanonical: "https://github.com/user/repo",
			wantHost:      "github.com",
			wantUser:      "user",
			wantRepo:      "repo",
			wantMatch:     true,
		},
		{
			name:          "URL with trailing slash",
			input:         "https://github.com/user/repo/",
			wantCanonical: "https://github.com/user/repo",
			wantMatch:     true,
		},
		{
			name:      "URL with trailing path segments (GitLab subgroup or deep path)",
			input:     "https://gitlab.com/group/subgroup/project",
			wantMatch: false,
		},
		{
			name:      "GitHub URL with trailing path",
			input:     "https://github.com/user/repo/releases/tag/v1.0",
			wantMatch: false,
		},
		{
			name:          "URL with .git suffix",
			input:         "https://github.com/user/repo.git",
			wantCanonical: "https://github.com/user/repo",
			wantRepo:      "repo",
			wantMatch:     true,
		},
		{
			name:          "bare host without scheme",
			input:         "github.com/user/repo",
			wantCanonical: "https://github.com/user/repo",
			wantMatch:     true,
		},
		{
			name:          "http scheme normalised to https",
			input:         "http://github.com/user/repo",
			wantCanonical: "https://github.com/user/repo",
			wantMatch:     true,
		},
		{
			name:          "URL with query string",
			input:         "https://github.com/user/repo?tab=readme",
			wantCanonical: "https://github.com/user/repo",
			wantMatch:     true,
		},
		{
			name:          "GitLab URL",
			input:         "https://gitlab.com/user/repo",
			wantCanonical: "https://gitlab.com/user/repo",
			wantHost:      "gitlab.com",
			wantMatch:     true,
		},
		{
			name:          "Codeberg URL",
			input:         "https://codeberg.org/user/repo",
			wantCanonical: "https://codeberg.org/user/repo",
			wantHost:      "codeberg.org",
			wantMatch:     true,
		},
		{
			name:      "plain text search",
			input:     "signal",
			wantMatch: false,
		},
		{
			name:      "host only, no path",
			input:     "https://github.com/",
			wantMatch: false,
		},
		{
			name:      "only one path segment (user profile, not repo)",
			input:     "https://github.com/user",
			wantMatch: false,
		},
		{
			name:      "empty string",
			input:     "",
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Parse(tt.input)
			if ok != tt.wantMatch {
				t.Fatalf("match: got %v, want %v", ok, tt.wantMatch)
			}
			if !ok {
				return
			}
			if tt.wantCanonical != "" && got.Canonical != tt.wantCanonical {
				t.Errorf("Canonical: got %q, want %q", got.Canonical, tt.wantCanonical)
			}
			if tt.wantHost != "" && got.Host != tt.wantHost {
				t.Errorf("Host: got %q, want %q", got.Host, tt.wantHost)
			}
			if tt.wantUser != "" && got.User != tt.wantUser {
				t.Errorf("User: got %q, want %q", got.User, tt.wantUser)
			}
			if tt.wantRepo != "" && got.Repo != tt.wantRepo {
				t.Errorf("Repo: got %q, want %q", got.Repo, tt.wantRepo)
			}
		})
	}
}
