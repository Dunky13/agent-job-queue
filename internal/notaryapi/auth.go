// Package notaryapi implements the CI notarization path without invoking notarytool.
package notaryapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

var uuidPattern = regexp.MustCompile(`^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`)
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)

type credentials struct {
	key             *ecdsa.PrivateKey
	keyID, issuerID string
}

func loadCredentials(getenv func(string) string) (*credentials, error) {
	keyID, issuerID := getenv("NOTARY_KEY_ID"), getenv("NOTARY_ISSUER_ID")
	// An issuer is mandatory: individual App Store Connect keys are not supported.
	if !keyIDPattern.MatchString(keyID) || !uuidPattern.MatchString(issuerID) || getenv("NOTARY_KEY_FILE") == "" {
		return nil, failure("invalid_credentials")
	}
	f, err := openRegular(getenv("NOTARY_KEY_FILE"))
	if err != nil {
		return nil, failure("invalid_key")
	}
	defer f.Close()
	data, err := readBounded(f, 16<<10)
	if err != nil {
		return nil, failure("invalid_key")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(strings.TrimSpace(string(rest))) != 0 || !strings.HasPrefix(strings.TrimSpace(string(data)), "-----BEGIN "+block.Type+"-----") {
		return nil, failure("invalid_key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, failure("invalid_key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, failure("invalid_key")
	}
	return &credentials{key: key, keyID: keyID, issuerID: issuerID}, nil
}

func (c *credentials) token(now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: c.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", c.keyID))
	if err != nil {
		return "", failure("invalid_key")
	}
	// Apple documents scope for App Store Connect GET operations, but not Notary
	// POST operations. Restrict this short-lived token at the HTTP boundary instead:
	// only the fixed production origin receives it, and redirects are forbidden.
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: c.issuerID, Audience: jwt.Audience{"appstoreconnect-v1"},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}).Serialize()
	if err != nil {
		return "", failure("invalid_key")
	}
	return token, nil
}
