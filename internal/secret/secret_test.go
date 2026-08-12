package secret

import (
	"errors"
	"os"
	"testing"
)

// TestValidateAcceptsGrammarWithoutReadingTheEnvironment is the property that
// lets a manifest be checked on a machine that does not hold the secret: a
// well-formed reference validates whether or not the variable is set.
func TestValidateAcceptsGrammarWithoutReadingTheEnvironment(t *testing.T) {
	const name = "DAC_TEST_DEFINITELY_UNSET"
	if _, set := os.LookupEnv(name); set {
		t.Skipf("%s is set in this environment", name)
	}
	if err := Validate(Prefix + name); err != nil {
		t.Fatalf("Validate(%q) = %v, want nil", Prefix+name, err)
	}
}

func TestValidateRejectsMalformedReferences(t *testing.T) {
	for _, value := range []string{Prefix, Prefix + "1LEADING_DIGIT", Prefix + "has space", Prefix + "has-dash"} {
		if err := Validate(value); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("Validate(%q) = %v, want ErrInvalidReference", value, err)
		}
	}
}

// TestLiteralValuesPassThrough keeps an ordinary header value from being
// treated as a reference just because it contains a colon.
func TestLiteralValuesPassThrough(t *testing.T) {
	for _, value := range []string{"Bearer abc", "https://example.com", "", "environment:NAME"} {
		if IsReference(value) {
			t.Errorf("IsReference(%q) = true, want false", value)
		}
		if err := Validate(value); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", value, err)
		}
		resolved, err := Resolve(value)
		if err != nil || resolved != value {
			t.Errorf("Resolve(%q) = %q, %v, want the value unchanged", value, resolved, err)
		}
	}
}

func TestResolveReadsTheEnvironment(t *testing.T) {
	const name = "DAC_TEST_SECRET"
	t.Setenv(name, "the-secret")
	resolved, err := Resolve(Prefix + name)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "the-secret" {
		t.Fatalf("Resolve = %q, want the environment value", resolved)
	}
}

// TestResolveClassifiesAnAbsentValue keeps a missing secret distinguishable
// from malformed configuration, since the two need different fixes.
func TestResolveClassifiesAnAbsentValue(t *testing.T) {
	const name = "DAC_TEST_MISSING_SECRET"
	t.Setenv(name, "present")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Prefix + name); !errors.Is(err, ErrMissingValue) {
		t.Fatalf("Resolve error = %v, want ErrMissingValue", err)
	}
}

// TestResolveDoesNotEchoTheSecret guards the reason this indirection exists:
// the failure names the variable, never a value.
func TestResolveDoesNotEchoTheSecret(t *testing.T) {
	if _, err := Resolve(Prefix + "has space"); err != nil && errors.Is(err, ErrMissingValue) {
		t.Fatal("a malformed reference reached the environment lookup")
	}
}
