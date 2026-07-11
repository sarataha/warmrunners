// Package webhook verifies inbound GitHub webhook deliveries.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// signaturePrefix is the scheme GitHub prepends to the hex-encoded HMAC in
// the X-Hub-Signature-256 header.
const signaturePrefix = "sha256="

var (
	// ErrMissingSignature is returned when the X-Hub-Signature-256 header is
	// empty.
	ErrMissingSignature = errors.New("webhook: X-Hub-Signature-256 missing")

	// ErrInvalidSignature is returned when the header is present but does not
	// match an HMAC-SHA256 of body computed with secret — this covers a bad
	// prefix, malformed hex, and a genuine signature mismatch alike.
	ErrInvalidSignature = errors.New("webhook: X-Hub-Signature-256 invalid")
)

// VerifySignature checks that sigHeader — the raw value of the
// X-Hub-Signature-256 header, including its "sha256=" prefix — is a valid
// HMAC-SHA256 of body computed with secret. Comparison uses hmac.Equal to
// avoid leaking timing information about how much of the signature matched.
func VerifySignature(body []byte, sigHeader string, secret []byte) error {
	if sigHeader == "" {
		return ErrMissingSignature
	}
	if !strings.HasPrefix(sigHeader, signaturePrefix) {
		return ErrInvalidSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHeader, signaturePrefix))
	if err != nil {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return ErrInvalidSignature
	}
	return nil
}
