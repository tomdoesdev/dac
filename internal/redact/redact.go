// Package redact removes credentials from text that reaches a user. Transport
// and parser diagnostics quote the URL they failed on, and a dac asset URL may
// carry a token in its query string, so the message must be cleaned before it
// is stored, logged, or printed.
package redact

import (
	"net/url"
	"regexp"
)

var urlToken = regexp.MustCompile(`https?://[^\s"']+`)

// URLs strips userinfo, query strings, and fragments from every URL in message
// while leaving the scheme, host, and path intact, so a diagnostic still says
// which host and artifact failed.
func URLs(message string) string {
	return urlToken.ReplaceAllStringFunc(message, func(value string) string {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return "remote URL"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
}

// Error wraps err so its message is redacted wherever it is rendered, while
// errors.Is and errors.As still reach the original cause. Use it at the point a
// third-party error enters dac's own error chain, rather than relying on the
// render boundary to catch every path.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return redactedError{err: err}
}

type redactedError struct{ err error }

func (err redactedError) Error() string { return URLs(err.err.Error()) }
func (err redactedError) Unwrap() error { return err.err }
