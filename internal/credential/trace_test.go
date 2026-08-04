package credential_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/credential"
	"github.com/tomdoesdev/dac/internal/debug"
)

// TestTraceNeverCarriesWhatAHelperReturned is the rule the trace exists under.
//
// A helper's answer is the secret. DAC already keeps it out of every error
// message and out of every log, and a trace added to show which helper answered
// must not be the thing that finally prints it. Names yes, values never.
func TestTraceNeverCarriesWhatAHelperReturned(t *testing.T) {
	const secret = "Bearer super-secret-token-value"
	helper := script(t, `printf '{"headers":{"Authorization":["`+secret+`"],"X-Api-Key":["another-secret"]}}'`+"\n")
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var trace bytes.Buffer
	resolver.Logger = debug.New(&trace, true)

	header, err := resolver.Headers(context.Background(), "https://files.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	// The headers still reach the caller. Tracing is not allowed to change what
	// the request carries, only what is said about it.
	if got := header.Get("Authorization"); got != secret {
		t.Fatalf("unexpected Authorization %q", got)
	}

	written := trace.String()
	for _, leaked := range []string{secret, "super-secret-token-value", "another-secret"} {
		if strings.Contains(written, leaked) {
			t.Fatalf("the trace leaked a credential value:\n%s", written)
		}
	}
	// What it does say is which helper answered and what it set, which is the
	// whole point of turning it on.
	for _, wanted := range []string{"Authorization", "X-Api-Key", "files.example.com", helper} {
		if !strings.Contains(written, wanted) {
			t.Fatalf("the trace omitted %q:\n%s", wanted, written)
		}
	}
}

// TestTraceSaysWhenNoHelperCoversAHost covers the answer an operator is
// usually looking for: not that a helper failed, but that none ran at all.
func TestTraceSaysWhenNoHelperCoversAHost(t *testing.T) {
	helper := script(t, `printf '{"headers":{"Authorization":["Bearer x"]}}'`+"\n")
	resolver, err := credential.New([]string{"files.example.com=" + helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var trace bytes.Buffer
	resolver.Logger = debug.New(&trace, true)

	if _, err := resolver.Headers(context.Background(), "https://other.example.com/one"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(trace.String(), "no credential helper for host") {
		t.Fatalf("the trace did not say the host was uncovered:\n%s", trace.String())
	}
}

// TestResolverWithoutALoggerTracesNothing keeps the zero value working: a
// resolver nobody handed a logger has to run rather than panic.
func TestResolverWithoutALoggerTracesNothing(t *testing.T) {
	helper := script(t, `printf '{"headers":{"Authorization":["Bearer x"]}}'`+"\n")
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Headers(context.Background(), "https://files.example.com/one"); err != nil {
		t.Fatal(err)
	}
}
