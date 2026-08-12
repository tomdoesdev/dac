package asset

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

const helloDigest = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

// TestNormalizeDigestClassifiesInvalidInput keeps callers independent from the
// digest parser's human-readable wording.
func TestNormalizeDigestClassifiesInvalidInput(t *testing.T) {
	if _, err := NormalizeDigest("md5:invalid"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("NormalizeDigest error = %v, want ErrInvalidDigest", err)
	}
}

// TestComputeDigest verifies that reader hashing produces dac's canonical
// digest format and preserves source read failures.
func TestComputeDigest(t *testing.T) {
	digest, err := computeDigest(strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if digest != helloDigest {
		t.Fatalf("digest = %q, want %q", digest, helloDigest)
	}

	wantErr := errors.New("test read failure")
	if _, err := computeDigest(iotest.ErrReader(wantErr)); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want error matching %v", err, wantErr)
	}
}

// TestCopyWithDigest verifies that staging preserves the bytes and byte count
// while calculating the digest without reading the source twice.
func TestCopyWithDigest(t *testing.T) {
	var destination bytes.Buffer
	size, digest, err := copyWithDigest(&destination, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len("hello")) {
		t.Fatalf("size = %d, want %d", size, len("hello"))
	}
	if destination.String() != "hello" {
		t.Fatalf("destination = %q, want %q", destination.String(), "hello")
	}
	if digest != helloDigest {
		t.Fatalf("digest = %q, want %q", digest, helloDigest)
	}
}
