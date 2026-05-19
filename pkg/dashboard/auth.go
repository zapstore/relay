package dashboard

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/pippellia-btc/nwt"
	"github.com/pippellia-btc/rely/v2"
)

type authValidator struct {
	Config
}

// IsAdmin reports whether the token signer is in AdminPubkeys.
func (v authValidator) IsAdmin(t nwt.Token) bool {
	return slices.Contains(v.AdminPubkeys, t.Signer)
}

// IsViewer reports whether the token signer is in ViewerPubkeys.
// Admin pubkeys are also considered viewers.
func (v authValidator) IsViewer(t nwt.Token) bool {
	return slices.Contains(v.ViewerPubkeys, t.Signer) ||
		slices.Contains(v.AdminPubkeys, t.Signer)
}

// validate checks the token's time claims, audience.
func (v authValidator) validate(t nwt.Token) error {
	if t.ID == "" {
		return nwt.ErrEmptyID
	}
	if err := nwt.ValidateTimeClaims(t, time.Minute); err != nil {
		return err
	}
	if len(t.Audience) > 0 {
		if !slices.Contains(t.Audience, v.Hostname) {
			return fmt.Errorf("%w: it doesn't contain an exact match of %q", nwt.ErrInvalidAudience, v.Hostname)
		}
	}
	return nil
}

// authenticate parses and validates the NWT from the request.
// On failure it penalizes the IP and writes the appropriate HTTP error.
// Returns the token and true on success, or zero value and false on failure.
func (d *T) authenticate(w http.ResponseWriter, r *http.Request) (nwt.Token, bool) {
	token, err := nwt.Parse(r)
	if err != nil {
		d.limiter.Penalize(rely.GetIP(r).Group(), 10)
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return nwt.Token{}, false
	}
	if err := d.auth.validate(token); err != nil {
		d.limiter.Penalize(rely.GetIP(r).Group(), 10)
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return nwt.Token{}, false
	}
	return token, true
}
