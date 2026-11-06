package blossom

import (
	"errors"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

func TestValidateAllowsUnsetBunny(t *testing.T) {
	c := NewConfig()
	c.Hostname = "localhost"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() with unset Bunny = %v", err)
	}
}

func TestValidateRejectsPartialBunny(t *testing.T) {
	c := NewConfig()
	c.Hostname = "localhost"
	c.Bunny.CDN = "cdn.example.com"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() with partial Bunny = nil, want error")
	}
}

func TestPruneIntervalMustBeSet(t *testing.T) {
	if got := NewConfig().PruneInterval; got != 0 {
		t.Fatalf("NewConfig().PruneInterval = %v, want 0", got)
	}

	tests := []struct {
		name      string
		value     string
		present   bool
		want      time.Duration
		wantMiss  bool
		wantEmpty bool
	}{
		{name: "unset", wantMiss: true},
		{name: "empty", value: "", present: true, wantEmpty: true},
		{name: "zero", value: "0", present: true},
		{name: "day", value: "24h", present: true, want: 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environ := map[string]string{}
			if tt.present {
				environ["BLOSSOM_PRUNE_INTERVAL"] = tt.value
			}
			cfg := NewConfig()
			cfg.PruneInterval = -1
			err := env.ParseWithOptions(&cfg, env.Options{Environment: environ})
			if tt.wantMiss {
				var got env.VarIsNotSetError
				if !errors.As(err, &got) || got.Key != "BLOSSOM_PRUNE_INTERVAL" {
					t.Fatalf("Parse() = %v, want BLOSSOM_PRUNE_INTERVAL unset", err)
				}
				return
			}
			if tt.wantEmpty {
				var got env.EmptyVarError
				if !errors.As(err, &got) || got.Key != "BLOSSOM_PRUNE_INTERVAL" {
					t.Fatalf("Parse(%q) = %v, want BLOSSOM_PRUNE_INTERVAL empty", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) = %v", tt.value, err)
			}
			if cfg.PruneInterval != tt.want {
				t.Fatalf("PruneInterval = %v, want %v", cfg.PruneInterval, tt.want)
			}
		})
	}
}
