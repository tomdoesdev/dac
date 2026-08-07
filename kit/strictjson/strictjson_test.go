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

func TestUnmarshalRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"duplicate key", `{"name":"a","name":"b"}`, "duplicate"},
		{"nested duplicate key", `{"inner":{"x":1,"x":2}}`, "duplicate"},
		{"unknown field", `{"name":"a","extra":1}`, "unknown field"},
		{"trailing value", `{"name":"a"} {"name":"b"}`, "more than one value"},
		{"not an object", `[1,2]`, "cannot unmarshal"},
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

func TestUnmarshalHasNoArbitraryNestingLimit(t *testing.T) {
	const depth = 256
	data := []byte(strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth))
	var value any
	if err := Unmarshal(data, &value, AllowUnknownMembers()); err != nil {
		t.Fatalf("%d levels of nesting returned %v", depth, err)
	}
}

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
		[]byte(`{"name":"a"}`),
		[]byte(`{"name":"a","name":"b"}`),
		[]byte(`[[[true]]]`),
		[]byte(`{"name":`),
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
