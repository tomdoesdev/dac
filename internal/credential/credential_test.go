package credential_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/credential"
)

// script writes an executable helper and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHeadersRunsTheHelperAndReturnsItsHeaders(t *testing.T) {
	helper := script(t, `printf '{"headers":{"Authorization":["Bearer token"]}}'`+"\n")
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	header, err := resolver.Headers(context.Background(), "https://files.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if got := header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("unexpected Authorization %q", got)
	}
}

func TestHeadersPassesTheRequestURI(t *testing.T) {
	helper := script(t, "cat > \"$TARGET\"\nprintf '{\"headers\":{}}'\n")
	target := filepath.Join(t.TempDir(), "request.json")
	t.Setenv("TARGET", target)
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Headers(context.Background(), "https://files.example.com/one"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"uri":"https://files.example.com/one"}`; string(data) != want {
		t.Fatalf("helper received %q, want %q", data, want)
	}
}

func TestHostSpecificHelperWinsOverAGeneralOne(t *testing.T) {
	general := script(t, `printf '{"headers":{"Authorization":["general"]}}'`+"\n")
	specific := script(t, `printf '{"headers":{"Authorization":["specific"]}}'`+"\n")
	resolver, err := credential.New([]string{general, "files.example.com=" + specific}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	matched, err := resolver.Headers(context.Background(), "https://files.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if got := matched.Get("Authorization"); got != "specific" {
		t.Fatalf("unexpected Authorization %q", got)
	}
	other, err := resolver.Headers(context.Background(), "https://other.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if got := other.Get("Authorization"); got != "general" {
		t.Fatalf("unexpected Authorization %q", got)
	}
}

func TestNoHelperCoversAnUnlistedHost(t *testing.T) {
	helper := script(t, `printf '{"headers":{"Authorization":["token"]}}'`+"\n")
	resolver, err := credential.New([]string{"files.example.com=" + helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	header, err := resolver.Headers(context.Background(), "https://other.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if header != nil {
		t.Fatalf("unexpected headers %#v", header)
	}
}

func TestNilResolverReportsNoHeaders(t *testing.T) {
	var resolver *credential.Resolver
	header, err := resolver.Headers(context.Background(), "https://files.example.com/one")
	if err != nil || header != nil {
		t.Fatalf("unexpected result %#v %v", header, err)
	}
}

func TestNewReportsNoResolverWithoutHelpers(t *testing.T) {
	resolver, err := credential.New(nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		t.Fatal("an empty specification list produced a resolver")
	}
}

func TestNewRejectsInvalidSpecifications(t *testing.T) {
	for _, specification := range []string{"", "   ", "files.example.com="} {
		if _, err := credential.New([]string{specification}, time.Minute); err == nil {
			t.Fatalf("%q was accepted", specification)
		}
	}
	if _, err := credential.New([]string{"helper"}, 0); err == nil {
		t.Fatal("a non-positive timeout was accepted")
	}
}

func TestHeadersReportsAFailingHelperWithItsStderr(t *testing.T) {
	helper := script(t, "echo 'no such vault entry' >&2\nexit 3\n")
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Headers(context.Background(), "https://files.example.com/one")
	if err == nil {
		t.Fatal("a failing helper did not produce an error")
	}
	if !strings.Contains(err.Error(), "no such vault entry") {
		t.Fatalf("error does not carry the helper's stderr: %v", err)
	}
}

func TestHeadersRejectsInvalidJSONWithoutQuotingIt(t *testing.T) {
	helper := script(t, `printf 'Bearer super-secret-value'`+"\n")
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Headers(context.Background(), "https://files.example.com/one")
	if err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error quoted the helper's output: %v", err)
	}
}

func TestHeadersStopsAHelperThatHangs(t *testing.T) {
	helper := script(t, "sleep 30\n")
	resolver, err := credential.New([]string{helper}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := resolver.Headers(context.Background(), "https://files.example.com/one"); err == nil {
		t.Fatal("a hanging helper did not produce an error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the timeout did not apply: waited %s", elapsed)
	}
}

// TestHeadersStopsAHelperWhoseChildHangs is the case that made the timeout a
// suggestion. A helper is usually a script, and a script that starts another
// program gives it the same output pipes. Killing the script leaves the child
// holding the write end, and the read that collects the helper's answer waits
// for whoever still holds it. Backgrounding the child takes the parent out of
// the picture entirely, so nothing but the pipe is left to wait on.
func TestHeadersStopsAHelperWhoseChildHangs(t *testing.T) {
	helper := script(t, "sleep 30 &\nexit 0\n")
	resolver, err := credential.New([]string{helper}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := resolver.Headers(context.Background(), "https://files.example.com/one"); err == nil {
		t.Fatal("a helper whose child holds the pipes did not produce an error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the helper outlived its bound: waited %s", elapsed)
	}
}

func TestEmptyHeadersReportNoCredentials(t *testing.T) {
	helper := script(t, `printf '{"headers":{}}'`+"\n")
	resolver, err := credential.New([]string{helper}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	header, err := resolver.Headers(context.Background(), "https://files.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if header != nil {
		t.Fatalf("unexpected headers %#v", header)
	}
}
