package command

import (
	"errors"
	"testing"
)

// TestSetErrorsAreClassifiable keeps malformed and duplicate automation input
// distinguishable from configuration failures discovered after parsing.
func TestSetErrorsAreClassifiable(t *testing.T) {
	if _, err := parseSets([]string{"missing-separator"}); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("invalid set error = %v, want ErrInvalidSet", err)
	}
	if _, err := parseSets([]string{"KEY=first", "KEY=second"}); !errors.Is(err, ErrDuplicateSet) {
		t.Fatalf("duplicate set error = %v, want ErrDuplicateSet", err)
	}
	sets, err := parseSets([]string{"LOCAL=one", "$.GLOBAL=two"})
	if err != nil {
		t.Fatal(err)
	}
	if sets.asset["LOCAL"] != "one" || sets.globals["GLOBAL"] != "two" {
		t.Fatalf("scoped sets = %#v", sets)
	}
}

// TestSetVariablesChecksEveryPreconditionFirst protects update's transactional
// behavior when one requested key is valid and a later key is a typo.
func TestSetVariablesChecksEveryPreconditionFirst(t *testing.T) {
	current := map[string]string{"EXISTING": "one"}
	requested := map[string]string{"EXISTING": "changed", "MISSING": "value"}
	if _, _, err := setVariables(current, requested, nil, "variable"); !errors.Is(err, ErrVariableNotFound) {
		t.Fatalf("set missing error = %v, want ErrVariableNotFound", err)
	}
	if current["EXISTING"] != "one" {
		t.Fatalf("failed edit changed variables: %#v", current)
	}
	updated, changed, err := setVariables(current, requested, map[string]struct{}{"MISSING": {}}, "variable")
	if err != nil || !changed || updated["EXISTING"] != "changed" || updated["MISSING"] != "value" {
		t.Fatalf("set referenced variable = %#v, %v, %v", updated, changed, err)
	}
}
