package relay

import (
	"bufio"
	_ "embed"
	"fmt"
	"strings"

	"github.com/avast/apkverifier/signingblock"
)

//go:embed signers.txt
var signersFile string

var storeSigners = mustParseSigners(signersFile)

// signerRule is one line of signers.txt.
type signerRule struct {
	kind  string
	value string
	label string
}

// thirdPartyError is an APK signed or delivered by a store. delivery is set
// for a mark the store adds when it delivers the APK, a source stamp or
// frosting: the developer may still hold the signing key, and a C1 proof
// shows they do.
type thirdPartyError struct {
	label    string
	mark     string
	delivery bool
}

func (e *thirdPartyError) Error() string {
	return fmt.Sprintf("third-party signer: %s, by its %s. Publish the APK you sign yourself", e.label, e.mark)
}

var markNames = map[string]string{
	"stamp":    "source stamp",
	"frosting": "Play frosting",
	"dn":       "certificate subject",
	"cert":     "certificate",
}

func mustParseSigners(text string) []signerRule {
	rules, err := parseSigners(text)
	if err != nil {
		panic(err)
	}
	return rules
}

func parseSigners(text string) ([]signerRule, error) {
	var rules []signerRule
	scanner := bufio.NewScanner(strings.NewReader(text))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("signers.txt:%d: want kind, value, label", n)
		}
		if _, ok := markNames[fields[0]]; !ok {
			return nil, fmt.Errorf("signers.txt:%d: unknown kind %q", n, fields[0])
		}
		value := fields[1]
		if fields[0] != "dn" {
			value = strings.ToLower(value)
		}
		rules = append(rules, signerRule{kind: fields[0], value: value, label: fields[2]})
	}
	return rules, scanner.Err()
}

// storeSigner returns the error for an APK that a rule matches, or nil.
// Every certificate in the chain is checked, not only the signer. A key rule
// wins over a delivery mark.
func storeSigner(rules []signerRule, chain signing, block *signingblock.VerificationResult) error {
	var delivered *thirdPartyError
	var stamp, frosting string
	if block != nil {
		if block.SourceStamp != nil && block.SourceStamp.Cert != nil {
			stamp = certHash(block.SourceStamp.Cert)
		}
		if block.Frosting != nil {
			frosting = strings.ToLower(block.Frosting.KeySha256)
		}
	}
	for _, rule := range rules {
		var hit bool
		switch rule.kind {
		case "stamp":
			hit = stamp != "" && stamp == rule.value
		case "frosting":
			hit = frosting != "" && frosting == rule.value
		case "dn":
			for _, c := range chain {
				hit = hit || c.Subject.String() == rule.value
			}
		case "cert":
			for _, c := range chain {
				hit = hit || certHash(c) == rule.value
			}
		}
		if !hit {
			continue
		}
		match := &thirdPartyError{label: rule.label, mark: markNames[rule.kind]}
		if rule.kind == "stamp" || rule.kind == "frosting" {
			match.delivery = true
			if delivered == nil {
				delivered = match
			}
			continue
		}
		return match
	}
	if delivered != nil {
		return delivered
	}
	return nil
}
