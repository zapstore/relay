package events

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

func TestValidateIdentityProof(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Unix(1_700_000_000, 0),
		NotAfter:     time.Unix(1_900_000_000, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	certHash := hexHash(sum[:])
	certB64 := base64.StdEncoding.EncodeToString(der)
	pub := "ab" + repeat("cd", 31)
	created := int64(1_700_000_100)
	expiry := int64(1_800_000_000)

	msg := sha256.Sum256([]byte(proofMessage(created, expiry, pub)))
	sig, err := ecdsa.SignASN1(rand.Reader, key, msg[:])
	if err != nil {
		t.Fatal(err)
	}
	active := &nostr.Event{
		Kind:      KindIdentityProof,
		PubKey:    pub,
		CreatedAt: nostr.Timestamp(created),
		Tags: nostr.Tags{
			{"d", certHash},
			{"signature", base64.StdEncoding.EncodeToString(sig)},
			{"expiry", "1800000000"},
			{"cert", certB64},
		},
	}
	if err := ValidateIdentityProof(active); err != nil {
		t.Fatal(err)
	}
	if !ProofActive(*active, time.Unix(created, 0)) {
		t.Fatal("proof should be active before expiry")
	}
	if ProofActive(*active, time.Unix(expiry, 0)) {
		t.Fatal("proof should be inactive at expiry")
	}

	missing := *active
	missing.Tags = nostr.Tags{
		{"d", certHash},
		{"signature", base64.StdEncoding.EncodeToString(sig)},
		{"expiry", "1800000000"},
	}
	if err := ValidateIdentityProof(&missing); err == nil {
		t.Fatal("active proof without cert should fail")
	}

	bad := *active
	bad.Tags = nostr.Tags{
		{"d", certHash},
		{"signature", base64.StdEncoding.EncodeToString([]byte("nope"))},
		{"expiry", "1800000000"},
		{"cert", certB64},
	}
	if err := ValidateIdentityProof(&bad); err == nil {
		t.Fatal("bad certificate signature should fail")
	}

	revoked := &nostr.Event{
		Kind:      KindIdentityProof,
		PubKey:    pub,
		CreatedAt: nostr.Timestamp(created + 1),
		Tags:      nostr.Tags{{"d", certHash}, {"revoked"}},
	}
	if err := ValidateIdentityProof(revoked); err != nil {
		t.Fatal(err)
	}
	if ProofActive(*revoked, time.Unix(created, 0)) {
		t.Fatal("revocation should be inactive")
	}

	withSig := *revoked
	withSig.Tags = append(withSig.Tags, nostr.Tag{"signature", "aa"})
	if err := ValidateIdentityProof(&withSig); err == nil {
		t.Fatal("revocation with signature should fail")
	}
}

func hexHash(sum []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(sum)*2)
	for i, b := range sum {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
