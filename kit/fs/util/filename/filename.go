// Package filename derives a safe local file name for a remote asset.
// A cache path is a digest, which is the right name for bytes and the wrong name for a tool that switches on an extension.
package filename

import (
	"mime"
	"net/url"
	"path"
	"strings"
)

// maxLength is NAME_MAX on the filesystems DAC runs on.
const maxLength = 255

// FromDisposition returns the file name a Content-Disposition header value gives, or an empty string when it gives none.
// mime.ParseMediaType decodes RFC filename parameters and prefers the encoded form.
func FromDisposition(header string) string {
	if strings.TrimSpace(header) == "" {
		return ""
	}
	_, parameters, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return Clean(parameters["filename"])
}

// FromURL returns the file name a URL suggests, or an empty string when it suggests none.
// It splits the escaped path and unescapes only the final segment, so a path holding an encoded separator keeps it.
func FromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base := path.Base(parsed.EscapedPath())
	unescaped, err := url.PathUnescape(base)
	if err != nil {
		return ""
	}
	return Clean(unescaped)
}

// Clean returns a name that is safe to use as one path element, or an empty string when the input cannot be made into one.
// It rejects rather than repairs.
func Clean(name string) string {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return ""
	// A name that is not one path element is not a file name, and treating it as one is how a script that writes to "./$(dac ...)" escapes its directory.
	case strings.ContainsAny(trimmed, `/\`):
		return ""
	case trimmed == "." || trimmed == "..":
		return ""
	// Length is counted in bytes because that is what the filesystem limits.
	case len(trimmed) > maxLength:
		return ""
	// A leading dash is a flag to the next tool in the pipeline, and DAC exists to be called from scripts that will not have quoted against it.
	case strings.HasPrefix(trimmed, "-"):
		return ""
	case strings.ContainsFunc(trimmed, isControl):
		return ""
	}
	return trimmed
}

// isControl reports the bytes that no file name should carry: the C0 range, DEL, and the C1 range that a Latin-1 header can smuggle through decoding.
func isControl(value rune) bool {
	return value < 0x20 || value == 0x7f || value >= 0x80 && value <= 0x9f
}
