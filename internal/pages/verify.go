package pages

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Sparkle signs the feed bytes that precede its trailing comment, and the
// comment's length field is that byte count. Checked against sign_update 2.10.1.
var feedSignatureComment = regexp.MustCompile(`<!-- sparkle-signatures:\nedSignature: ([A-Za-z0-9+/]{86}==)\nlength: ([0-9]+)\n-->\s*$`)

// loadPublicKey reads the one-line base64 public key the app trusts.
func loadPublicKey(path string) (ed25519.PublicKey, error) {
	body, err := boundedFile(path, 256)
	if err != nil {
		return nil, errors.New("The Sparkle public key file " + path + " is missing or unreadable. " + docsPointer)
	}
	text := strings.TrimSpace(string(body))
	key, err := base64.StdEncoding.DecodeString(text)
	if err != nil || strings.ContainsAny(text, "\r\n") || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("The Sparkle public key file " + path + " must hold one base64 line with a 32-byte EdDSA key. " + docsPointer)
	}
	return ed25519.PublicKey(key), nil
}

// verifySignatures proves the app, which trusts only this public key, accepts
// both the enclosure signature over the exact ZIP and the feed signature.
func verifySignatures(key ed25519.PublicKey, archive, signedFeed []byte) error {
	if len(key) != ed25519.PublicKeySize {
		return errors.New("The Sparkle public key is not loaded")
	}
	item, _, err := parseFeed(signedFeed)
	if err != nil {
		return err
	}
	archiveSignature, err := base64.StdEncoding.DecodeString(item.Enclosure.Signature)
	if err != nil || !ed25519.Verify(key, archive, archiveSignature) {
		return errors.New("The archive signature does not verify against the Sparkle public key; SPARKLE_ED_PRIVATE_KEY may not match it. " + docsPointer)
	}
	match := feedSignatureComment.FindSubmatchIndex(signedFeed)
	if match == nil {
		return errors.New("The feed signature comment is missing or malformed")
	}
	length, err := strconv.Atoi(string(signedFeed[match[4]:match[5]]))
	if err != nil || length != match[0] {
		return errors.New("The feed signature does not cover the feed content")
	}
	feedSignature, err := base64.StdEncoding.DecodeString(string(signedFeed[match[2]:match[3]]))
	if err != nil || !ed25519.Verify(key, signedFeed[:length], feedSignature) {
		return errors.New("The feed signature does not verify against the Sparkle public key; SPARKLE_ED_PRIVATE_KEY may not match it. " + docsPointer)
	}
	return nil
}
