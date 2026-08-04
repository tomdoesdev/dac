package output

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
)

func TestWriterWritesHumanOutput(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, false)
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

func TestWriterWritesOnlyVersionedJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	writer := New(&stdout, &stderr, true)
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
	writer := New(&stdout, &stderr, true)
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
