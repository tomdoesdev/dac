package cli

import (
	"bytes"
	"context"
	"testing"
)

type optionalString struct{ value string }

func (value *optionalString) String() string   { return value.value }
func (value *optionalString) Type() string     { return "optional-string" }
func (value *optionalString) IsBoolFlag() bool { return true }
func (value *optionalString) Set(input string) error {
	value.value = input
	return nil
}

type optionalValueHandler struct {
	Value optionalString `flag:"value"`
	Name  string         `arg:"name"`
}

func (*optionalValueHandler) Description() string       { return "Tests an optional custom flag value" }
func (*optionalValueHandler) Run(context.Context) error { return nil }

// TestCustomOptionalValueCanOmitItsArgument captures the IsBoolFlag convention
// used by flag.Value implementations with optional values. A bare --value must
// receive "true" without consuming the following required positional.
func TestCustomOptionalValueCanOmitItsArgument(t *testing.T) {
	handler := &optionalValueHandler{}
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddCommand("run <name>", handler)
	if err := app.Run([]string{"run", "--value", "artifact"}); err != nil {
		t.Fatalf("run with bare optional value: %v", err)
	}
	if handler.Value.value != "true" || handler.Name != "artifact" {
		t.Fatalf("value=%q name=%q", handler.Value.value, handler.Name)
	}
}
