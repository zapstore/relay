package analytics

import "testing"

func TestValidateAllowsUnsetAddress(t *testing.T) {
	c := NewConfig()
	if c.Enabled() {
		t.Fatal("Enabled() with unset address = true, want false")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() with unset address = %v", err)
	}
}

func TestValidateRejectsInvalidAddress(t *testing.T) {
	c := NewConfig()
	c.Address = "not a tcp address"
	if !c.Enabled() {
		t.Fatal("Enabled() with address set = false, want true")
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() with invalid address = nil, want error")
	}
}
