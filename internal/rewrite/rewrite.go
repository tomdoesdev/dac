// Package rewrite maps canonical asset URLs onto the URLs that DAC requests.
//
// A manifest records where an asset comes from upstream. A site that proxies
// its downloads needs those requests to go somewhere else, and editing every
// project manifest to say so spreads one deployment's topology across every
// repository that uses it. A rewrite config keeps the two apart: the manifest
// and the lock file stay canonical, and the request URL is decided here.
//
// Redirecting a download cannot change what DAC accepts. Every pull still
// checks the locked digest, so a mirror that serves the wrong bytes fails the
// same way a corrupted transfer does. The exception is lock, which has no
// expected digest yet unless the asset carries a publisher integrity value; a
// rewrite config is trusted input for the same reason a manifest URL is.
package rewrite

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/tom/dac/internal/urlpolicy"
)

// ErrBlocked marks a URL that the config refuses. It wraps the URL policy error
// so callers treat it as a permanent rejection rather than a transient failure.
var ErrBlocked = fmt.Errorf("%w: the rewrite config blocks this host", urlpolicy.ErrNotPermitted)

// ErrAmbiguousRewrite marks a URL that matches multiple rewrite rules.
var ErrAmbiguousRewrite = errors.New("more than one rewrite rule matches this URL")

// Result describes the request URL that a config produced.
type Result struct {
	URL               string
	Rewritten         bool
	AllowInsecureHTTP bool
	Blocked           bool
}

// Config holds URL rewrite rules and request host policy rules.
type Config struct {
	rewrites      []rewriteRule
	allows        []string
	blocks        []string
	allowInsecure bool
}

type rewriteRule struct {
	pattern     *regexp.Regexp
	replacement string
}

// Load reads a rewrite config. A missing file is not an error: it reports no
// config, which leaves every asset URL untouched.
func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	config, err := Parse(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// Parse reads directives, one per line. Blank lines and lines starting with #
// are ignored.
//
//	allow <host|*>                   permit requests to this host
//	block <host|*>                   refuse this host
//	rewrite <pattern> <replacement>  send matching requests elsewhere
//	allow_insecure_http              permit HTTP for rewritten URLs
//
// Rewrite rules apply before host policy rules. One URL must not match more
// than one rewrite rule. An allow match overrides all block matches.
func Parse(reader io.Reader) (*Config, error) {
	config := &Config{}
	scanner := bufio.NewScanner(reader)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if err := config.add(strings.Fields(text)); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return config, nil
}

func (config *Config) add(fields []string) error {
	switch directive := fields[0]; directive {
	case "allow_insecure_http":
		if len(fields) != 1 {
			return errors.New("allow_insecure_http takes no arguments")
		}
		config.allowInsecure = true
	case "allow", "block":
		if len(fields) != 2 {
			return fmt.Errorf("%s takes one host", directive)
		}
		host := strings.ToLower(fields[1])
		if directive == "allow" {
			config.allows = append(config.allows, host)
		} else {
			config.blocks = append(config.blocks, host)
		}
	case "rewrite":
		if len(fields) != 3 {
			return errors.New("rewrite takes one pattern and one replacement")
		}
		pattern, err := regexp.Compile(fields[1])
		if err != nil {
			return fmt.Errorf("rewrite pattern is not valid: %w", err)
		}
		config.rewrites = append(config.rewrites, rewriteRule{pattern: pattern, replacement: fields[2]})
	default:
		return fmt.Errorf("unknown directive %q", directive)
	}
	return nil
}

type decision struct {
	result Result
	host   string
}

// Evaluate reports the rule result without returning ErrBlocked for a blocked URL.
func (config *Config) Evaluate(rawURL string) (Result, error) {
	value, err := config.evaluate(rawURL)
	return value.result, err
}

// Apply returns the URL that DAC should request.
func (config *Config) Apply(rawURL string) (Result, error) {
	value, err := config.evaluate(rawURL)
	if err != nil {
		return Result{}, err
	}
	if value.result.Blocked {
		return Result{}, blockedError(value.host)
	}
	return value.result, nil
}

// Check applies host policy to one URL. It does not apply rewrite rules because
// a redirect must not start a rewrite chain.
func (config *Config) Check(value *url.URL) error {
	if value == nil {
		return errors.New("the request URL is nil")
	}
	host := strings.ToLower(value.Hostname())
	if config.blocksHost(host) {
		return blockedError(host)
	}
	return nil
}

// evaluate rewrites a URL before it applies host policy. It reports a block
// decision as data so the rewrite command can show the selected target.
//
// Rewrite patterns match against "host/path" with any query string appended,
// which is the same subject Bazel's downloader config uses, so a pattern can
// key on the host without having to allow for the scheme.
func (config *Config) evaluate(rawURL string) (decision, error) {
	unchanged := Result{URL: rawURL}
	if config == nil || len(config.rewrites)+len(config.allows)+len(config.blocks) == 0 {
		return decision{result: unchanged}, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return decision{}, err
	}
	subject := parsed.Host + parsed.EscapedPath()
	if parsed.RawQuery != "" {
		subject += "?" + parsed.RawQuery
	}
	match := -1
	for index, current := range config.rewrites {
		if !current.pattern.MatchString(subject) {
			continue
		}
		if match >= 0 {
			return decision{}, fmt.Errorf("%w: %s", ErrAmbiguousRewrite, rawURL)
		}
		match = index
	}

	result := unchanged
	target := parsed
	if match >= 0 {
		current := config.rewrites[match]
		rawTarget := current.pattern.ReplaceAllString(subject, current.replacement)
		if !strings.Contains(rawTarget, "://") {
			rawTarget = parsed.Scheme + "://" + rawTarget
		}
		target, err = url.Parse(rawTarget)
		if err != nil {
			return decision{}, err
		}
		result = Result{URL: rawTarget, Rewritten: true, AllowInsecureHTTP: config.allowInsecure}
	}

	host := strings.ToLower(target.Hostname())
	result.Blocked = config.blocksHost(host)
	return decision{result: result, host: host}, nil
}

// blocksHost applies fixed allow-over-block precedence. Separate scans keep
// this decision independent from the directive order.
func (config *Config) blocksHost(host string) bool {
	if config == nil {
		return false
	}
	for _, pattern := range config.allows {
		if matchesHost(host, pattern) {
			return false
		}
	}
	for _, pattern := range config.blocks {
		if matchesHost(host, pattern) {
			return true
		}
	}
	return false
}

func blockedError(host string) error {
	return fmt.Errorf("%w: %s", ErrBlocked, host)
}

// matchesHost lets "*" match all hosts. Other patterns match an exact host or
// any subdomain of that host.
func matchesHost(host, pattern string) bool {
	return pattern == "*" || host == pattern || strings.HasSuffix(host, "."+pattern)
}
