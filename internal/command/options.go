package command

import "github.com/tomdoesdev/dac/internal/asset"

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
