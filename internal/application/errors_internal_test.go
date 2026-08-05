package application

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
)

// requestError stands in for the HTTP client's error so this test does not
// import the package that imports this one.
type requestError struct {
	url    string
	status int
	err    error
}

func (value *requestError) Error() string      { return fmt.Sprintf("%s: %v", value.url, value.err) }
func (value *requestError) Unwrap() error      { return value.err }
func (value *requestError) RequestURL() string { return value.url }
func (value *requestError) StatusCode() int    { return value.status }

// TestNetworkErrorReportsARefusedHost covers the whole reason the refusal is a
// typed error: somebody reading the message has to be told which host to trust,
// and a JSON consumer has to be able to read it without parsing the message.
func TestNetworkErrorReportsARefusedHost(t *testing.T) {
	// The transport wraps every round-trip error in a *url.Error, which answers
	// to net.Error, so this is the shape the classifier actually receives.
	refusal := &requestError{
		url: "https://example.com/asset",
		err: &url.Error{Op: "Get", URL: "https://example.com/asset", Err: &HostError{Host: "example.com"}},
	}
	value := fault.As(networkError(refusal))
	if value.Code != "host_not_trusted" {
		t.Fatalf("code = %q, want host_not_trusted", value.Code)
	}
	if !strings.Contains(value.Message, "dac trust add example.com") {
		t.Fatalf("message = %q, want the command that fixes it", value.Message)
	}
	if value.Details["host"] != "example.com" || value.Details["url"] != "https://example.com/asset" {
		t.Fatalf("details = %v, want both the refused host and the asset URL", value.Details)
	}
}

// TestNetworkErrorPrefersARefusalOverATimeout pins the case ordering.
// A *url.Error is a net.Error, so a classifier that asked about timeouts first
// would answer "timeout" for anything a decorator refused inside one.
func TestNetworkErrorPrefersARefusalOverATimeout(t *testing.T) {
	var network net.Error = &url.Error{Op: "Get", URL: "https://example.com/asset", Err: &HostError{Host: "example.com"}}
	if !errors.As(error(network), new(*HostError)) {
		t.Fatal("the test error no longer carries a refusal")
	}
	if value := fault.As(networkError(network)); value.Code != "host_not_trusted" {
		t.Fatalf("code = %q, want host_not_trusted", value.Code)
	}
}
