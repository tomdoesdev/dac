package strictjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type document struct {
	Name  string         `json:"name"`
	Count int            `json:"count"`
	Inner map[string]any `json:"inner,omitempty"`
}

// TestUnmarshalRejectsMalformedInput defines the stricter-than-encoding/json
// contract used for DAC-owned files. Ambiguous, schema-incompatible, concatenated,
// or syntactically incomplete documents must fail with a useful reason instead
// of being partially accepted.
func TestUnmarshalRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		// Duplicate object members are ambiguous because decoder choice would
		// otherwise decide whether the first or last value wins.
		{"duplicate key", `{"name":"a","name":"b"}`, "duplicate"},
		// Duplicate detection must recurse rather than inspect only the root object.
		{"nested duplicate key", `{"inner":{"x":1,"x":2}}`, "duplicate"},
		// Unknown fields usually indicate a typo or a file from an unsupported version.
		{"unknown field", `{"name":"a","extra":1}`, "unknown field"},
		// Exactly one JSON value is allowed; silently ignoring a second document is unsafe.
		{"trailing value", `{"name":"a"} {"name":"b"}`, "more than one value"},
		// The strict scanner must still preserve Go destination-type validation.
		{"not an object", `[1,2]`, "cannot unmarshal"},
		// Incomplete syntax must surface the underlying end-of-input signal.
		{"truncated", `{"name":`, "EOF"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var value document
			err := Unmarshal([]byte(testCase.data), &value)
			if err == nil {
				t.Fatalf("%q was accepted", testCase.data)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

// TestUnmarshalAcceptsValidInput prevents strictness from becoming blanket
// rejection. Normal nested objects, arrays, and dynamic map contents must decode,
// and identical key names in separate sibling objects are not duplicates.
func TestUnmarshalAcceptsValidInput(t *testing.T) {
	var value document
	if err := Unmarshal([]byte(`{"name":"a","count":2,"inner":{"x":1,"y":[1,2,{"z":3}]}}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.Name != "a" || value.Count != 2 {
		t.Fatalf("unexpected value: %+v", value)
	}
	// Duplicate detection must not reject repeated keys in sibling objects.
	if err := Unmarshal([]byte(`{"inner":{"a":{"x":1},"b":{"x":2}}}`), &value); err != nil {
		t.Fatalf("sibling objects with the same key were rejected: %v", err)
	}
}

// TestUnmarshalHasNoArbitraryNestingLimit guards the iterative token walk from
// acquiring a shallower limit than encoding/json. A deeply nested but valid
// document should be governed by the standard decoder, not an incidental strict
// validation threshold.
func TestUnmarshalHasNoArbitraryNestingLimit(t *testing.T) {
	const depth = 256
	data := []byte(strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth))
	var value any
	if err := Unmarshal(data, &value, AllowUnknownMembers()); err != nil {
		t.Fatalf("%d levels of nesting returned %v", depth, err)
	}
}

// TestUnmarshalReportsCaseInsensitiveStructFieldNames prevents encoding/json's
// default case folding from hiding schema mistakes. The default error must name
// the exact-match problem and its position, while an explicit compatibility
// option retains support for legacy documents.
func TestUnmarshalReportsCaseInsensitiveStructFieldNames(t *testing.T) {
	var value document
	err := Unmarshal([]byte(`{"Name":"a"}`), &value)
	if err == nil || !strings.Contains(err.Error(), "does not match a field name exactly") {
		t.Fatalf("case-insensitive name returned %v", err)
	}
	if !strings.Contains(err.Error(), "1:") {
		t.Fatalf("case error does not carry a position: %v", err)
	}

	if err := Unmarshal([]byte(`{"Name":"a"}`), &value, MatchCaseInsensitiveNames()); err != nil {
		t.Fatalf("compatibility option rejected a legacy name: %v", err)
	}
}

// TestUnmarshalFollowsEmbeddedStructFields ensures schema discovery mirrors Go's
// JSON field promotion rules. Promoted exact names must decode, and their folded
// variants must receive the same strict rejection as direct fields.
func TestUnmarshalFollowsEmbeddedStructFields(t *testing.T) {
	type embedded struct {
		Name string `json:"name"`
	}
	type outer struct {
		embedded
		Count int `json:"count"`
	}

	var value outer
	if err := Unmarshal([]byte(`{"name":"a","count":2}`), &value); err != nil {
		t.Fatal(err)
	}
	err := Unmarshal([]byte(`{"NAME":"a"}`), &value)
	if err == nil || !strings.Contains(err.Error(), "does not match a field name exactly") {
		t.Fatalf("folded embedded name returned %v", err)
	}
}

// TestUnmarshalFollowsMapValueTypes prevents strict validation from stopping at
// dynamic map keys. The keys are unrestricted, but each typed struct value must
// still enforce exact JSON field names.
func TestUnmarshalFollowsMapValueTypes(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}
	type collection struct {
		Items map[string]item `json:"items"`
	}

	var value collection
	err := Unmarshal([]byte(`{"items":{"first":{"NAME":"a"}}}`), &value)
	if err == nil || !strings.Contains(err.Error(), "does not match a field name exactly") {
		t.Fatalf("folded map value field returned %v", err)
	}
}

// TestOptionsLoosenOnlyTheirOwnRules protects option independence. Allowing
// forward-compatible fields and legacy casing must not implicitly disable the
// duplicate-member check, whose ambiguity remains unsafe in every mode.
func TestOptionsLoosenOnlyTheirOwnRules(t *testing.T) {
	var value document
	if err := Unmarshal([]byte(`{"name":"a","newField":1}`), &value, AllowUnknownMembers()); err != nil {
		t.Fatalf("unknown-member option rejected input: %v", err)
	}
	err := Unmarshal([]byte(`{"name":"a","name":"b"}`), &value, AllowUnknownMembers(), MatchCaseInsensitiveNames())
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("options accepted duplicate members: %v", err)
	}
}

// TestReadFileReportsMissingFiles keeps filesystem error identity intact. DAC
// callers use os.IsNotExist to distinguish an absent optional file from malformed
// JSON, so ReadFile must not obscure that sentinel while adding decode behavior.
func TestReadFileReportsMissingFiles(t *testing.T) {
	var value document
	err := ReadFile(filepath.Join(t.TempDir(), "absent.json"), &value)
	if !os.IsNotExist(err) {
		t.Fatalf("missing file returned %v", err)
	}
}

// FuzzUnmarshal keeps the token walk parser-safe. Every accepted document must
// also be acceptable to encoding/json, which owns the actual Go decoding.
func FuzzUnmarshal(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"name":"a"}`),            // Baseline valid object.
		[]byte(`{"name":"a","name":"b"}`), // Strict-only duplicate rejection.
		[]byte(`[[[true]]]`),              // Nested arrays exercise container tracking.
		[]byte(`{"name":`),                // Truncation exercises incomplete token handling.
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var got any
		err := Unmarshal(data, &got, AllowUnknownMembers(), MatchCaseInsensitiveNames())
		if err != nil {
			return
		}
		var standard any
		if err := json.Unmarshal(data, &standard); err != nil {
			t.Fatalf("strictjson accepted input that encoding/json rejected: %q: %v", data, err)
		}
	})
}
