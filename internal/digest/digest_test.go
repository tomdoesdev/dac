package digest

import (
	"strings"
	"testing"
)

// The digest of the empty input, which pins the encoding of every result.
const emptyDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestBytesPinsTheDigestEncoding(t *testing.T) {
	if Bytes(nil) != emptyDigest {
		t.Fatalf("empty digest is %q", Bytes(nil))
	}
	if value := Bytes([]byte("locked bytes")); !strings.HasPrefix(value, Prefix) {
		t.Fatalf("digest %q has no prefix", value)
	}
}

func TestHexValidatesFormat(t *testing.T) {
	valid := emptyDigest
	hexValue, err := Hex(valid)
	if err != nil {
		t.Fatal(err)
	}
	if hexValue != strings.TrimPrefix(valid, Prefix) {
		t.Fatalf("hex is %q", hexValue)
	}

	// Uppercase is rejected so that one set of bytes has exactly one address.
	cases := []string{
		"",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"sha512:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"sha256:E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855",
		"sha256:e3b0c4",
		"sha256:" + strings.Repeat("g", 64),
	}
	for _, value := range cases {
		if _, err := Hex(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
}

func TestCanonicalAcceptsBothDigestForms(t *testing.T) {
	// The same all-zero SHA-256 written as hex and as Subresource Integrity.
	const hexForm = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	const sriForm = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, value := range []string{hexForm, sriForm, "  " + sriForm + "  "} {
		canonical, err := Canonical(value)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if canonical != hexForm {
			t.Fatalf("%q became %q, want %q", value, canonical, hexForm)
		}
	}
}

func TestCanonicalRoundTripsARealDigest(t *testing.T) {
	value := Bytes([]byte("asset bytes"))
	canonical, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != value {
		t.Fatalf("canonical form changed %q to %q", value, canonical)
	}
}

func TestCanonicalRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{
		"",
		"sha256-not base64",
		"sha256-AAAA",
		"sha512:0000",
		"0000000000000000000000000000000000000000000000000000000000000000",
	} {
		if _, err := Canonical(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
}
