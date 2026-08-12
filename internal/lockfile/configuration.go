package lockfile

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/manifest"
)

const configurationDigestVersion = "dac-lock-configuration-v1"

// ConfigurationDigest hashes every manifest-side input that defines an
// accepted source policy. Environment-backed header values remain references;
// their resolved secrets never enter persistent state.
func ConfigurationDigest(value manifest.ResolvedAsset) string {
	hasher := sha256.New()
	writeDigestField(hasher, configurationDigestVersion)
	writeDigestField(hasher, value.URL)
	writeDigestField(hasher, value.File)
	writeDigestField(hasher, value.ResolvedURL)
	writeDigestField(hasher, value.ResolvedFile)
	pin := value.Pin
	if pin != "" {
		pin, _ = asset.NormalizeDigest(pin)
	}
	writeDigestField(hasher, pin)
	writeDigestMap(hasher, value.Variables, false)
	writeDigestMap(hasher, value.Headers, true)
	writeDigestInt64(hasher, value.Transfer.MaxSize)
	writeDigestInt64(hasher, int64(value.Transfer.IdleTimeout))
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

// writeDigestMap provides stable map ordering and optional case-folding while
// retaining explicit field boundaries in the digest input.
func writeDigestMap(hasher hash.Hash, values map[string]string, foldKeys bool) {
	keys := make([]string, 0, len(values))
	normalized := make(map[string]string, len(values))
	for key, value := range values {
		if foldKeys {
			key = strings.ToLower(key)
		}
		keys = append(keys, key)
		normalized[key] = value
	}
	sort.Strings(keys)
	writeDigestInt64(hasher, int64(len(keys)))
	for _, key := range keys {
		writeDigestField(hasher, key)
		writeDigestField(hasher, normalized[key])
	}
}

func writeDigestField(hasher hash.Hash, value string) {
	writeDigestInt64(hasher, int64(len(value)))
	_, _ = hasher.Write([]byte(value))
}

func writeDigestInt64(hasher hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = hasher.Write(encoded[:])
}
