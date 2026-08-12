package asset

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strings"
)

var sha256Pattern = regexp.MustCompile(`^sha256:([0-9A-Fa-f]{64})$`)

// ErrInvalidDigest marks a value outside dac's canonical SHA-256 format.
var ErrInvalidDigest = errors.New("expected sha256 followed by 64 hexadecimal characters")

// NormalizeDigest parses dac's supported digest format and returns its
// canonical lowercase spelling for stable serialization and comparison.
func NormalizeDigest(value string) (string, error) {
	matches := sha256Pattern.FindStringSubmatch(value)
	if matches == nil {
		return "", ErrInvalidDigest
	}
	return "sha256:" + strings.ToLower(matches[1]), nil
}

// computeDigest consumes source and returns its canonical SHA-256 digest.
func computeDigest(source io.Reader) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, source); err != nil {
		return "", err
	}
	return formatSHA256Digest(hasher.Sum(nil)), nil
}

// copyWithDigest streams source to destination while computing the digest in
// the same pass, avoiding a second read of downloaded content.
func copyWithDigest(destination io.Writer, source io.Reader) (int64, string, error) {
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(destination, hasher), source)
	if err != nil {
		return size, "", err
	}
	return size, formatSHA256Digest(hasher.Sum(nil)), nil
}

// formatSHA256Digest gives raw SHA-256 bytes their canonical serialized form.
func formatSHA256Digest(sum []byte) string {
	return "sha256:" + hex.EncodeToString(sum)
}
