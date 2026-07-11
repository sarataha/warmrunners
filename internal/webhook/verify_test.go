package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_ValidBody(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"action":"opened"}`)
	sig := sign(secret, body)

	if err := VerifySignature(body, sig, secret); err != nil {
		t.Errorf("VerifySignature() = %v, want nil", err)
	}
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"action":"opened"}`)
	sig := sign(secret, body)

	tampered := []byte(`{"action":"closed"}`)
	if err := VerifySignature(tampered, sig, secret); err != ErrInvalidSignature {
		t.Errorf("VerifySignature() = %v, want ErrInvalidSignature", err)
	}
}

func TestVerifySignature_MissingHeader(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"action":"opened"}`)

	if err := VerifySignature(body, "", secret); err != ErrMissingSignature {
		t.Errorf("VerifySignature() = %v, want ErrMissingSignature", err)
	}
}

func TestVerifySignature_WrongPrefix(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := "sha1=" + hex.EncodeToString(mac.Sum(nil))

	if err := VerifySignature(body, sig, secret); err != ErrInvalidSignature {
		t.Errorf("VerifySignature() = %v, want ErrInvalidSignature", err)
	}
}

// TestVerifySignature_ConstantTime asserts VerifySignature uses hmac.Equal
// rather than a length-based shortcut. Two mismatched signatures of equal
// length (same hex-encoded length as a genuine sha256 HMAC) must both be
// rejected with ErrInvalidSignature, never something that would signal an
// early length exit.
func TestVerifySignature_ConstantTime(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"action":"opened"}`)

	// 32 zero bytes hex-encoded: same length as a real sha256 sum, but wrong.
	zeroSig := signaturePrefix + hex.EncodeToString(make([]byte, sha256.Size))
	// 32 bytes of 0xff hex-encoded: also same length, also wrong.
	ffBytes := make([]byte, sha256.Size)
	for i := range ffBytes {
		ffBytes[i] = 0xff
	}
	ffSig := signaturePrefix + hex.EncodeToString(ffBytes)

	if err := VerifySignature(body, zeroSig, secret); err != ErrInvalidSignature {
		t.Errorf("VerifySignature(zeroSig) = %v, want ErrInvalidSignature", err)
	}
	if err := VerifySignature(body, ffSig, secret); err != ErrInvalidSignature {
		t.Errorf("VerifySignature(ffSig) = %v, want ErrInvalidSignature", err)
	}
}
