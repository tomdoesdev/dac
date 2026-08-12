package asset

import (
	"encoding/hex"
	"errors"
	"hash"

	"regexp"
	"strings"
)

var sha256Pattern = regexp.MustCompile(`^sha256:([0-9A-Fa-f]{64})$`)

// NormalizeDigest parses dac's supported digest format and returns its
// canonical lowercase spelling for stable serialization and comparison.
func NormalizeDigest(value string) (string, error) {
	matches := sha256Pattern.FindStringSubmatch(value)
	if matches == nil {
		return "", errors.New("expected sha256 followed by 64 hexadecimal characters")
	}
	return "sha256:" + strings.ToLower(matches[1]), nil
}

func DigestFromHash(h hash.Hash) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
