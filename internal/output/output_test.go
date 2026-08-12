package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/project"
)

// TestSanitizeErrorRemovesURLSecrets keeps diagnostics from becoming a secret
// exfiltration path. User info, query parameters, and fragments commonly carry
// credentials, while the scheme, host, and path are still useful for debugging.
func TestSanitizeErrorRemovesURLSecrets(t *testing.T) {
	message := "request https://user:password@example.com/artifact?token=secret#fragment failed"
	got := sanitizeError(message)
	if want := "request https://example.com/artifact failed"; got != want {
		t.Fatalf("sanitizeError = %q, want %q", got, want)
	}
}

// TestOptionsRejectJSONAndQuiet pins an intentionally invalid output contract:
// JSON promises machine-readable output while quiet promises no output. DAC
// rejects the ambiguous combination instead of choosing one mode implicitly.
func TestOptionsRejectJSONAndQuiet(t *testing.T) {
	if err := (Options{JSON: true, Quiet: true}).Validate(); !errors.Is(err, ErrConflictingModes) {
		t.Fatalf("Validate error = %v, want ErrConflictingModes", err)
	}
}

func TestSuccessWarningsReachHumanAndJSONOutputSafely(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		name := "human"
		if jsonMode {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			writer := New(&Options{JSON: jsonMode}, stdout, stderr)
			warning := "cleanup failed for https://example.com/file?token=secret"
			if err := writer.Success("lock", project.Paths{Root: "/project"}, nil, []string{warning}); err != nil {
				t.Fatal(err)
			}
			combined := stdout.String() + stderr.String()
			if strings.Contains(combined, "secret") {
				t.Fatalf("warning leaked URL query: %q", combined)
			}
			if jsonMode {
				var value struct {
					Warnings []string `json:"warnings"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &value); err != nil || len(value.Warnings) != 1 {
					t.Fatalf("JSON warning = %#v, %v", value, err)
				}
			} else if !strings.Contains(stderr.String(), "Warning:") {
				t.Fatalf("human stderr = %q", stderr.String())
			}
		})
	}
}

// TestHumanOutputUsesSemanticColors verifies DAC consumes cli's public styling
// API while keeping message content and non-status details unchanged.
func TestHumanOutputUsesSemanticColors(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr)

	if err := writer.Success("pull", project.Paths{}, []Result{{Name: "asset", Status: "downloaded"}}, []string{"check cache"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Status(project.Paths{}, []Result{
		{Name: "healthy", Status: "verified"},
		{Name: "broken", Status: "invalid", Reason: "digest mismatch"},
		{Name: "absent", Status: "missing"},
	}, []Orphan{{Kind: "lock", Name: "old", Reason: "unused"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "\x1b[32m") || !strings.Contains(stdout.String(), "\x1b[31m") || !strings.Contains(stdout.String(), "\x1b[33m") {
		t.Fatalf("semantic stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "\x1b[33mWarning:") || !strings.Contains(stderr.String(), "check cache") {
		t.Fatalf("semantic stderr = %q", stderr.String())
	}
}

// TestStructuredAndQuietOutputNeverUseColor protects machine output even when
// the environment asks terminal-oriented code to force ANSI sequences.
func TestStructuredAndQuietOutputNeverUseColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")

	jsonOutput := &bytes.Buffer{}
	writer := New(&Options{JSON: true}, jsonOutput, &bytes.Buffer{})
	if err := writer.Success("pull", project.Paths{}, []Result{{Name: "asset", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOutput.String(), "\x1b[") || !json.Valid(jsonOutput.Bytes()) {
		t.Fatalf("JSON output = %q", jsonOutput.String())
	}

	quietOutput := &bytes.Buffer{}
	writer = New(&Options{Quiet: true}, quietOutput, &bytes.Buffer{})
	if err := writer.Success("pull", project.Paths{}, []Result{{Name: "asset", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if quietOutput.Len() != 0 {
		t.Fatalf("quiet output = %q", quietOutput.String())
	}
}
