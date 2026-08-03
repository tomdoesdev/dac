package rewrite_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tom/dac/internal/rewrite"
	"github.com/tom/dac/internal/urlpolicy"
)

func parse(t *testing.T, text string) *rewrite.Config {
	t.Helper()
	config, err := rewrite.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return config
}

func TestApplyRewritesMatchingURLs(t *testing.T) {
	config := parse(t, "rewrite ^example\\.com/(.*)$ mirror.internal/upstream/$1\n")
	result, err := config.Apply("https://example.com/geo/database.bin")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://mirror.internal/upstream/geo/database.bin" {
		t.Fatalf("unexpected URL %q", result.URL)
	}
	if !result.Rewritten {
		t.Fatal("result is not marked as rewritten")
	}
}

func TestApplyKeepsQueryInTheSubject(t *testing.T) {
	config := parse(t, "rewrite ^example\\.com/get\\?id=(.*)$ mirror.internal/$1\n")
	result, err := config.Apply("https://example.com/get?id=7")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://mirror.internal/7" {
		t.Fatalf("unexpected URL %q", result.URL)
	}
}

func TestApplyKeepsTheOriginalSchemeUnlessTheReplacementSetsOne(t *testing.T) {
	config := parse(t, "rewrite ^example\\.com/(.*)$ http://mirror.internal/$1\n")
	result, err := config.Apply("https://example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "http://mirror.internal/one" {
		t.Fatalf("unexpected URL %q", result.URL)
	}
}

func TestApplyLeavesUnmatchedURLsAlone(t *testing.T) {
	config := parse(t, "rewrite ^example\\.com/(.*)$ mirror.internal/$1\n")
	result, err := config.Apply("https://other.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://other.com/one" || result.Rewritten {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestHostPolicyAppliesToTheRewrittenTarget(t *testing.T) {
	config := parse(t, "allow source.example.com\nblock mirror.internal\nrewrite ^source\\.example\\.com/(.*)$ mirror.internal/$1\n")
	result, err := config.Evaluate("https://source.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://mirror.internal/one" || !result.Rewritten || !result.Blocked {
		t.Fatalf("unexpected result %#v", result)
	}
	if _, err := config.Apply("https://source.example.com/one"); !errors.Is(err, rewrite.ErrBlocked) || !strings.Contains(err.Error(), "mirror.internal") {
		t.Fatalf("expected ErrBlocked for mirror.internal, got %v", err)
	}
}

func TestRewriteCanMoveABlockedSourceToAnAllowedTarget(t *testing.T) {
	config := parse(t, "block source.example.com\nallow mirror.internal\nrewrite ^source\\.example\\.com/(.*)$ mirror.internal/$1\n")
	result, err := config.Apply("https://source.example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://mirror.internal/one" || !result.Rewritten {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestBlockRefusesAHostAndItsSubdomains(t *testing.T) {
	config := parse(t, "block example.com\n")
	for _, value := range []string{"https://example.com/one", "https://files.example.com/one"} {
		if _, err := config.Apply(value); !errors.Is(err, rewrite.ErrBlocked) {
			t.Fatalf("%s: expected ErrBlocked, got %v", value, err)
		} else if !errors.Is(err, urlpolicy.ErrNotPermitted) {
			t.Fatalf("%s: block is not a policy rejection: %v", value, err)
		}
	}
	if _, err := config.Apply("https://notexample.com/one"); err != nil {
		t.Fatalf("block matched an unrelated host: %v", err)
	}
}

func TestEvaluateReportsABlockedURL(t *testing.T) {
	config := parse(t, "block example.com\n")
	result, err := config.Evaluate("https://example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Blocked || result.URL != "https://example.com/one" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestWildcardBlockRefusesAllOtherHosts(t *testing.T) {
	for _, rules := range []string{
		"allow trusted.example.com\nblock *\n",
		"block *\nallow trusted.example.com\n",
	} {
		config := parse(t, rules)
		if _, err := config.Apply("https://trusted.example.com/one"); err != nil {
			t.Fatalf("allow exception failed: %v", err)
		}
		for _, value := range []string{"https://other.example.com/one", "https://127.0.0.1/one", "https://[::1]/one"} {
			if _, err := config.Apply(value); !errors.Is(err, rewrite.ErrBlocked) {
				t.Fatalf("%s: expected ErrBlocked, got %v", value, err)
			}
		}
	}
}

func TestWildcardAllowOverridesASpecificBlock(t *testing.T) {
	for _, rules := range []string{
		"block denied.example.com\nallow *\n",
		"allow *\nblock denied.example.com\n",
	} {
		config := parse(t, rules)
		if _, err := config.Apply("https://denied.example.com/one"); err != nil {
			t.Fatalf("allow rule did not override the block rule: %v", err)
		}
	}
}

func TestMultipleMatchingRewritesFailWithoutOrderDependence(t *testing.T) {
	rules := []string{
		"rewrite ^example\\.com/(.*)$ mirror.internal/$1\nrewrite ^example\\.com/one$ mirror.internal/one\n",
		"rewrite ^example\\.com/one$ mirror.internal/one\nrewrite ^example\\.com/(.*)$ mirror.internal/$1\n",
	}
	for _, text := range rules {
		config := parse(t, text)
		if _, err := config.Apply("https://example.com/one"); !errors.Is(err, rewrite.ErrAmbiguousRewrite) {
			t.Fatalf("expected ErrAmbiguousRewrite, got %v", err)
		}
	}
}

func TestAllowInsecureHTTPAppliesOnlyToRewrittenURLs(t *testing.T) {
	config := parse(t, "allow_insecure_http\nrewrite ^example\\.com/(.*)$ http://mirror.internal/$1\n")
	rewritten, err := config.Apply("https://example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if !rewritten.AllowInsecureHTTP {
		t.Fatal("rewritten URL did not opt into insecure HTTP")
	}
	untouched, err := config.Apply("https://other.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if untouched.AllowInsecureHTTP {
		t.Fatal("an untouched URL opted into insecure HTTP")
	}
}

func TestParseIgnoresBlankLinesAndComments(t *testing.T) {
	config := parse(t, "\n# a comment\n\n  block example.com  \n")
	if _, err := config.Apply("https://example.com/one"); !errors.Is(err, rewrite.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
}

func TestParseRejectsInvalidDirectives(t *testing.T) {
	for _, text := range []string{
		"nonsense example.com\n",
		"rewrite onlyonefield\n",
		"rewrite ^( replacement\n",
		"allow\n",
		"block one two\n",
		"allow_insecure_http yes\n",
	} {
		if _, err := rewrite.Parse(strings.NewReader(text)); err == nil {
			t.Fatalf("%q parsed without error", text)
		}
	}
}

func TestNilConfigLeavesURLsAlone(t *testing.T) {
	var config *rewrite.Config
	result, err := config.Apply("https://example.com/one")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://example.com/one" || result.Rewritten {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestLoadReportsNoConfigForAMissingFile(t *testing.T) {
	config, err := rewrite.Load(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if config != nil {
		t.Fatal("a missing file produced a config")
	}
}

func TestLoadReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rewrite")
	if err := os.WriteFile(path, []byte("block example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := rewrite.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Apply("https://example.com/one"); !errors.Is(err, rewrite.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
}
