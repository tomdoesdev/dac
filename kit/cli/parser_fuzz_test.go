package cli

import "testing"

// FuzzParsePattern hardens command registration against arbitrary struct-tag or
// application-provided patterns. Invalid patterns may return errors, but no
// byte sequence should panic or trap the parser in a non-terminating state.
func FuzzParsePattern(f *testing.F) {
	for _, seed := range []string{
		"remote add <name>", // A valid multi-token path with a required argument.
		"run [files...]",    // A valid optional variadic argument exercises both suffixes.
		"",                  // The smallest invalid pattern covers empty-token handling.
		"run <name> again",  // A literal after an argument is structurally invalid.
		"%flags% run",       // The internal marker must not be accepted as public syntax.
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		_, _ = parsePattern(pattern)
	})
}

// FuzzParseFlagTag guards reflection-time flag discovery. Tags are supplied by
// developers rather than end users, but malformed tags must still fail cleanly
// because they are parsed during application startup.
func FuzzParseFlagTag(f *testing.F) {
	for _, seed := range []string{
		"verbose,v", // Valid long and short names.
		"output",    // Valid long-only name.
		"-,o",       // Valid short-only sentinel and short name.
		"-",         // Incomplete short-only spelling.
		"bad=name",  // '=' conflicts with long --name=value parsing.
		"value,=",   // Invalid short-name rune.
		"",          // Empty tags must not cause indexing panics.
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tag string) {
		_, _, _ = parseFlagTag(tag)
	})
}
