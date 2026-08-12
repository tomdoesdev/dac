package command

import (
	"fmt"

	"github.com/tomdoesdev/dac/internal/asset"
)

// singleValue rejects repeated scalar options instead of silently allowing the
// final spelling to win, which keeps update intent unambiguous.
type singleValue struct {
	name  string
	value string
	set   bool
}

func newSingleValue(name string) *singleValue { return &singleValue{name: name} }
func (value *singleValue) String() string     { return value.value }
func (*singleValue) Type() string             { return "string" }
func (value *singleValue) Set(input string) error {
	if value.set {
		return fmt.Errorf("%w: %s", ErrOptionRepeated, value.name)
	}
	value.value, value.set = input, true
	return nil
}

// pinValue represents the two legal --pin forms. pflag asks Set("true") for a
// bare optional flag, which intentionally means calculate rather than a digest.
type pinValue struct {
	calculate bool
	digest    string
	set       bool
}

func (value *pinValue) String() string { return value.digest }
func (*pinValue) Type() string         { return "sha256" }
func (*pinValue) IsBoolFlag() bool     { return true }
func (value *pinValue) Set(input string) error {
	if value.set {
		return ErrPinRepeated
	}
	value.set = true
	if input == "true" {
		value.calculate = true
		return nil
	}
	digest, err := asset.NormalizeDigest(input)
	if err != nil {
		return err
	}
	value.digest = digest
	return nil
}
