package output

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/cli"
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

// TestDownloadProgressCompletesOnStderrWithoutDuplicatingStdout verifies the
// interactive contract while ensuring URL paths and query secrets stay hidden.
func TestDownloadProgressCompletesOnStderrWithoutDuplicatingStdout(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr)
	writer.throbberMode = cli.ThrobberAlways
	writer.throbberInterval = time.Hour

	err := writer.WithDownloadProgress(context.Background(), "artifact", "asset.zip", "https://example.com/private/asset.zip?token=secret", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Success("pull", project.Paths{}, []Result{
		{Name: "artifact", Status: "downloaded"},
		{Name: "cached", Status: "verified"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "verified cached\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	progress := stderr.String()
	if !strings.Contains(progress, "Downloading asset.zip from example.com… (0s)") || !strings.Contains(progress, "✔ asset.zip downloaded from example.com (0s)") {
		t.Fatalf("stderr = %q", progress)
	}
	if strings.Contains(progress, "private") || strings.Contains(progress, "secret") {
		t.Fatalf("progress leaked source details: %q", progress)
	}
}

// TestRedirectedDownloadProgressFallsBackToSuccess keeps ordinary human output
// useful in logs and pipelines where rewriting a terminal line is unavailable.
func TestRedirectedDownloadProgressFallsBackToSuccess(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr)
	if err := writer.WithDownloadProgress(context.Background(), "artifact", "asset.zip", "https://example.com/asset.zip", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := writer.Success("pull", project.Paths{}, []Result{{Name: "artifact", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "downloaded artifact\n"; got != want || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want %q and empty", got, stderr.String(), want)
	}
}

// TestFailedDownloadProgressClearsWithoutSuccess prevents a failed transfer
// from leaving a misleading completion record before the normal error output.
func TestFailedDownloadProgressClearsWithoutSuccess(t *testing.T) {
	stderr := &bytes.Buffer{}
	writer := New(&Options{}, &bytes.Buffer{}, stderr)
	writer.throbberMode = cli.ThrobberAlways
	writer.throbberInterval = time.Hour
	want := errors.New("download failed")
	err := writer.WithDownloadProgress(context.Background(), "artifact", "asset.zip", "https://example.com/asset.zip", func(context.Context) error { return want })
	if !errors.Is(err, want) || strings.Contains(stderr.String(), "✔") || !strings.HasSuffix(stderr.String(), "\r\x1b[2K") {
		t.Fatalf("error=%v stderr=%q", err, stderr.String())
	}
}

// TestStructuredAndQuietOutputNeverUseColor protects machine output even when
// the environment asks terminal-oriented code to force ANSI sequences.
func TestStructuredAndQuietOutputNeverUseColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")

	jsonOutput := &bytes.Buffer{}
	jsonError := &bytes.Buffer{}
	writer := New(&Options{JSON: true}, jsonOutput, jsonError)
	writer.throbberMode = cli.ThrobberAlways
	if err := writer.WithDownloadProgress(context.Background(), "asset", "asset.zip", "https://example.com/asset.zip", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := writer.Success("pull", project.Paths{}, []Result{{Name: "asset", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOutput.String(), "\x1b[") || !json.Valid(jsonOutput.Bytes()) || jsonError.Len() != 0 {
		t.Fatalf("JSON output=%q stderr=%q", jsonOutput.String(), jsonError.String())
	}

	quietOutput := &bytes.Buffer{}
	quietError := &bytes.Buffer{}
	writer = New(&Options{Quiet: true}, quietOutput, quietError)
	writer.throbberMode = cli.ThrobberAlways
	if err := writer.WithDownloadProgress(context.Background(), "asset", "asset.zip", "https://example.com/asset.zip", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := writer.Success("pull", project.Paths{}, []Result{{Name: "asset", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if quietOutput.Len() != 0 || quietError.Len() != 0 {
		t.Fatalf("quiet stdout=%q stderr=%q", quietOutput.String(), quietError.String())
	}
}
