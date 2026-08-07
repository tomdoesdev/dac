package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/output/style"
)

func TestWriterWritesHumanOutput(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, false, style.Palette{})
	if err := writer.Success("path", map[string]string{"path": "/cache/object"}, "Found asset."); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Found asset.\n" || stderr.Len() != 0 {
		t.Fatalf("unexpected success output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	err := &fault.Error{Code: "content_mismatch", Message: "The content changed."}
	if writeErr := writer.Failure("pull", err); writeErr != nil {
		t.Fatal(writeErr)
	}
	if stdout.Len() != 0 || stderr.String() != "Error: The content changed.\n" {
		t.Fatalf("unexpected failure output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// A failure is the one line this writer composes rather than passes through,
// and the label is the part of it worth finding. The message beside it is the
// part that has to be read, so it stays as it was written -- including the
// cause, which is where a colour would land in the middle of a URL.
func TestWriterColoursOnlyTheErrorLabel(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, false, style.New(&stderr, style.Always))
	err := fault.Wrap("network_error", "The asset request failed.", errors.New("https://example.com/db: HTTP 404"))
	if writeErr := writer.Failure("pull", err); writeErr != nil {
		t.Fatal(writeErr)
	}
	written := stderr.String()
	if !strings.HasPrefix(written, "\x1b[") {
		t.Fatalf("the error label was not styled: %q", written)
	}
	if plain := sequences.ReplaceAllString(written, ""); plain != "Error: "+err.Error()+"\n" {
		t.Fatalf("colour changed the message: %q", plain)
	}
	if strings.Count(written, "\x1b[") != 2 {
		t.Fatalf("colour reached past the label: %q", written)
	}
}

// JSON is parsed, so nothing may colour it -- not the document on standard
// output, and not the human error message that is not written at all.
func TestJSONModeIsNeverColoured(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, true, style.New(&stderr, style.Always))
	if err := writer.Failure("pull", fault.New("content_mismatch", "The content changed.")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\x1b") || stderr.Len() != 0 {
		t.Fatalf("JSON mode wrote escapes: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

var sequences = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestWriterWritesOnlyVersionedJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, true, style.Palette{})
	if err := writer.Success("path", map[string]string{"path": "/cache/object"}, "Found asset."); err != nil {
		t.Fatal(err)
	}
	document := decodeOne(t, stdout.Bytes())
	if document["outputVersion"] != float64(Version) || document["ok"] != true || document["command"] != "path" {
		t.Fatalf("unexpected document: %#v", document)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON success wrote stderr: %q", stderr.String())
	}
	stdout.Reset()
	err := &fault.Error{Code: "content_mismatch", Message: "The content changed.", Details: map[string]any{"asset": "geo"}}
	if writeErr := writer.Failure("pull", err); writeErr != nil {
		t.Fatal(writeErr)
	}
	document = decodeOne(t, stdout.Bytes())
	errorValue := document["error"].(map[string]any)
	if document["ok"] != false || errorValue["code"] != "content_mismatch" {
		t.Fatalf("unexpected document: %#v", document)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON failure wrote stderr: %q", stderr.String())
	}
}

// A consumer parsing the error envelope must always find a details object, even
// for an error that carries no details of its own.
func TestJSONFailureAlwaysCarriesADetailsObject(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, true, style.Palette{})
	if err := writer.Failure("pull", fault.New("content_mismatch", "The content changed.")); err != nil {
		t.Fatal(err)
	}
	errorValue := decodeOne(t, stdout.Bytes())["error"].(map[string]any)
	details, ok := errorValue["details"].(map[string]any)
	if !ok || details == nil {
		t.Fatalf("details is not an object: %#v", errorValue["details"])
	}
	if len(details) != 0 {
		t.Fatalf("unexpected details: %#v", details)
	}
}

func decodeOne(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout contains more than one JSON document: %v", err)
	}
	return document
}
