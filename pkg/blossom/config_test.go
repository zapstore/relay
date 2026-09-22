package blossom

import "testing"

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
