package relay

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/avast/apkverifier"
)

const maxAPK = 300_000_000

// signing is the certificate chain an APK is signed with: its v3 rotation
// lineage, oldest first, ending at the signer. It is never empty.
type signing []*x509.Certificate

// readSigning verifies an APK and returns its chain. An APK with more than one
// current signer is refused.
func readSigning(r io.ReadSeeker) (signing, error) {
	res, err := apkverifier.VerifyReader(r, nil)
	if err != nil {
		return nil, fmt.Errorf("apk: %w", err)
	}
	if len(res.SignerCerts) != 1 {
		return nil, fmt.Errorf("apk: found %d signers", len(res.SignerCerts))
	}
	_, signer := apkverifier.PickBestApkCert(res.SignerCerts)
	if signer == nil {
		return nil, errors.New("apk: no signing certificate")
	}
	if res.SigningBlockResult == nil || res.SigningBlockResult.SigningLineage == nil {
		return signing{signer}, nil
	}
	nodes := res.SigningBlockResult.SigningLineage.Nodes
	if len(nodes) == 0 {
		return signing{signer}, nil
	}
	if !nodes[len(nodes)-1].SigningCert.Equal(signer) {
		return nil, errors.New("apk: signing lineage does not end at the signer")
	}
	chain := make(signing, 0, len(nodes))
	for _, node := range nodes {
		chain = append(chain, node.SigningCert)
	}
	return chain, nil
}

// signer is the hash of the certificate that signed the APK.
func (s signing) signer() string {
	return certHash(s[len(s)-1])
}

// cert returns the certificate in the chain with this hash, or nil. A match
// means the APK is signed by that certificate or a successor in its lineage.
func (s signing) cert(hash string) *x509.Certificate {
	for _, c := range s {
		if certHash(c) == hash {
			return c
		}
	}
	return nil
}

func (s signing) der() [][]byte {
	out := make([][]byte, len(s))
	for i, c := range s {
		out[i] = c.Raw
	}
	return out
}

func parseSigning(ders [][]byte) (signing, error) {
	if len(ders) == 0 {
		return nil, errors.New("apk signing has no certificate")
	}
	out := make(signing, len(ders))
	for i, der := range ders {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("apk signing: %w", err)
		}
		out[i] = c
	}
	return out, nil
}

func certHash(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}
