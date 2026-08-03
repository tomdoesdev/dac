// Package digest calculates and checks SHA-256 digests.
package digest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// Prefix marks the canonical digest form that DAC stores and compares.
	Prefix = "sha256:"
	// SRIPrefix marks the Subresource Integrity form that DAC accepts as input.
	SRIPrefix = "sha256-"
)

// Bytes returns the digest for data.
func Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return Prefix + hex.EncodeToString(sum[:])
}

// Canonical accepts a digest in either the canonical "sha256:<hex>" form or the
// Subresource Integrity "sha256-<base64>" form and returns the canonical form.
//
// DAC stores and compares only the canonical form. It accepts the SRI form
// because publishers and other tools quote digests that way.
func Canonical(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, SRIPrefix) {
		canonical := strings.ToLower(trimmed)
		if _, err := Hex(canonical); err != nil {
			return "", err
		}
		return canonical, nil
	}
	encoded := strings.TrimPrefix(trimmed, SRIPrefix)
	sum, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("integrity is not valid base64: %w", err)
	}
	if len(sum) != sha256.Size {
		return "", fmt.Errorf("integrity must encode %d bytes, not %d", sha256.Size, len(sum))
	}
	return Prefix + hex.EncodeToString(sum), nil
}

// Hex validates a canonical SHA-256 digest and returns its hexadecimal part.
func Hex(value string) (string, error) {
	if !strings.HasPrefix(value, Prefix) {
		return "", fmt.Errorf("digest must start with %q or %q", Prefix, SRIPrefix)
	}
	hexValue := strings.TrimPrefix(value, Prefix)
	if len(hexValue) != sha256.Size*2 || strings.ToLower(hexValue) != hexValue {
		return "", fmt.Errorf("digest must contain 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(hexValue); err != nil {
		return "", fmt.Errorf("digest is not valid: %w", err)
	}
	return hexValue, nil
}
