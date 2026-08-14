package output

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/redact"
)

// TestSanitizeErrorRemovesURLSecrets keeps diagnostics from becoming a secret
// exfiltration path. User info, query parameters, and fragments commonly carry
// credentials, while the scheme, host, and path are still useful for debugging.
func TestSanitizeErrorRemovesURLSecrets(t *testing.T) {
	message := "request https://user:password@example.com/artifact?token=secret#fragment failed"
	got := redact.URLs(message)
	if want := "request https://example.com/artifact failed"; got != want {
		t.Fatalf("redact.URLs = %q, want %q", got, want)
	}
}

// TestFormatRecoveryQuotesOpaqueNames ensures recovery instructions remain safe
// to paste into a POSIX shell. Asset names are deliberately opaque and may begin
// with a dash or contain shell metacharacters, so the renderer must quote them
// rather than let a shell reinterpret them as options or syntax.
func TestFormatRecoveryQuotesOpaqueNames(t *testing.T) {
	targeted := fault.Recovery{Command: "pull", Flags: []string{"--update-lockfile"}, Assets: []string{"-scope/pkg'$`"}}
	if got, want := formatRecovery(targeted), `run: dac pull --update-lockfile -- '-scope/pkg'"'"'$`+"`'"; got != want {
		t.Fatalf("formatRecovery = %q, want %q", got, want)
	}
	if got, want := formatRecovery(fault.Recovery{Command: "pull", Flags: []string{"--update-lockfile"}}), "run: dac pull --update-lockfile"; got != want {
		t.Fatalf("formatRecovery = %q, want %q", got, want)
	}
	if got := formatRecovery(fault.Recovery{}); got != "" {
		t.Fatalf("formatRecovery(empty) = %q, want empty", got)
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
			if err := writer.Success("lock", "/project", nil, []string{warning}); err != nil {
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
// API while keeping message content unchanged.
func TestHumanOutputUsesSemanticColors(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr)

	if err := writer.Success("pull", "", []Result{{Name: "asset", Status: "downloaded"}}, []string{"check cache"}); err != nil {
		t.Fatal(err)
	}
	writer.Error(fault.NewIntegrityError(errors.New("digest mismatch")))
	if !strings.Contains(stdout.String(), "\x1b[32m") {
		t.Fatalf("semantic stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "\x1b[31mError:") || !strings.Contains(stderr.String(), "\x1b[33mWarning:") || !strings.Contains(stderr.String(), "check cache") {
		t.Fatalf("semantic stderr = %q", stderr.String())
	}
}

// TestDownloadProgressCompletesOnStderrWithoutDuplicatingStdout verifies the
// interactive contract while ensuring URL paths and query secrets stay hidden.
func TestDownloadProgressCompletesOnStderrWithoutDuplicatingStdout(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr, withProgress(progressAlways, time.Millisecond))

	reported, err := writer.WithDownloadProgress(context.Background(), "asset.zip", "https://example.com/private/asset.zip?token=secret", func(_ context.Context, progress DownloadProgress) error {
		// Keep the transfer active through several refreshes so the test covers
		// both mpb's running and completed presentations.
		progress(0, 1024)
		time.Sleep(5 * time.Millisecond)
		progress(512, 1024)
		time.Sleep(5 * time.Millisecond)
		progress(1024, 1024)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reported {
		t.Fatal("an animated download did not report that it announced itself")
	}
	if err := writer.Success("pull", "", []Result{
		{Name: "artifact", Status: "downloaded", Reported: reported},
		{Name: "cached", Status: "verified"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "verified cached\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	progress := stderr.String()
	if !strings.Contains(progress, "asset.zip") || !strings.Contains(progress, "example.com") || !strings.Contains(progress, "[") || !strings.Contains(progress, "]") || !strings.Contains(progress, "100%") || !strings.Contains(progress, "✔  asset.zip") {
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
	reported, err := writer.WithDownloadProgress(context.Background(), "asset.zip", "https://example.com/asset.zip", func(context.Context, DownloadProgress) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if reported {
		t.Fatal("a non-animated download claimed to have announced itself")
	}
	if err := writer.Success("pull", "", []Result{{Name: "artifact", Status: "downloaded", Reported: reported}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "downloaded artifact\n"; got != want || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want %q and empty", got, stderr.String(), want)
	}
}

// TestFailedDownloadProgressLeavesNoSuccess prevents a failed transfer from
// leaving a misleading completion record before the normal error output.
func TestFailedDownloadProgressLeavesNoSuccess(t *testing.T) {
	stderr := &bytes.Buffer{}
	writer := New(&Options{}, &bytes.Buffer{}, stderr, withProgress(progressAlways, time.Millisecond))
	want := errors.New("download failed")
	_, err := writer.WithDownloadProgress(context.Background(), "asset.zip", "https://example.com/asset.zip", func(context.Context, DownloadProgress) error { return want })
	if !errors.Is(err, want) || strings.Contains(stderr.String(), "✔") || strings.Contains(stderr.String(), "downloaded") {
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
	writer := New(&Options{JSON: true}, jsonOutput, jsonError, withProgress(progressAlways, defaultProgressRefresh))
	if _, err := writer.WithDownloadProgress(context.Background(), "asset.zip", "https://example.com/asset.zip", func(context.Context, DownloadProgress) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := writer.Success("pull", "", []Result{{Name: "asset", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOutput.String(), "\x1b[") || !json.Valid(jsonOutput.Bytes()) || jsonError.Len() != 0 {
		t.Fatalf("JSON output=%q stderr=%q", jsonOutput.String(), jsonError.String())
	}

	quietOutput := &bytes.Buffer{}
	quietError := &bytes.Buffer{}
	writer = New(&Options{Quiet: true}, quietOutput, quietError, withProgress(progressAlways, defaultProgressRefresh))
	if _, err := writer.WithDownloadProgress(context.Background(), "asset.zip", "https://example.com/asset.zip", func(context.Context, DownloadProgress) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := writer.Success("pull", "", []Result{{Name: "asset", Status: "downloaded"}}, nil); err != nil {
		t.Fatal(err)
	}
	if quietOutput.Len() != 0 || quietError.Len() != 0 {
		t.Fatalf("quiet stdout=%q stderr=%q", quietOutput.String(), quietError.String())
	}
}
