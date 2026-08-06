package getit

import (
	"net/http"
	"strings"
	"time"
)

// Validator is what an origin says a set of bytes is: an entity tag, a
// modification time, or both.
//
// One type answers three questions. Is this still what I hold, which is
// If-None-Match and If-Modified-Since on a request. Is every range of this
// split download the same bytes, which is If-Match and If-Unmodified-Since on
// each range. And, in a later version, may I resume onto what I have, which is
// If-Range.
//
// Compare the fields rather than the struct. A Validator that came from an
// origin also keeps the exact text of the header it was read from, so that a
// date this package echoes back is the date the origin wrote.
type Validator struct {
	// ETag is the entity tag as the origin sent it, quotes and any W/ prefix
	// included. It is stored whole because that is what goes back on the wire.
	ETag string
	// LastModified is the modification time the origin reported, or the zero
	// time when it reported none.
	LastModified time.Time

	// lastModifiedText is the Last-Modified header as the origin spelled it.
	// HTTP allows three date formats and asks a sender to write only the first,
	// but a precondition that fails wrongly fails a whole download, so a date
	// this package sends back is the one it was given rather than a reformatting
	// of it.
	lastModifiedText string
}

// Empty reports a validator that says nothing about any bytes.
func (validator Validator) Empty() bool {
	return validator.ETag == "" && validator.LastModified.IsZero()
}

// Strong reports an entity tag that names one exact set of bytes.
//
// A weak tag says two responses are equivalent, not that they are the same
// bytes, which is the only question a byte range asks. So a weak tag is enough
// to revalidate what a caller holds and not enough to hold a split download
// together.
func (validator Validator) Strong() bool {
	return validator.ETag != "" && !strings.HasPrefix(validator.ETag, "W/")
}

// validatorFrom reads what a response says its bytes are.
func validatorFrom(header http.Header) Validator {
	validator := Validator{ETag: strings.TrimSpace(header.Get("ETag"))}
	text := strings.TrimSpace(header.Get("Last-Modified"))
	if text == "" {
		return validator
	}
	when, err := http.ParseTime(text)
	if err != nil {
		// A date this package cannot read is a date it cannot compare or send.
		return validator
	}
	validator.LastModified, validator.lastModifiedText = when, text
	return validator
}

// revalidate asks an origin whether the bytes a caller holds are still current.
// Both preconditions go out when there are both to send: an origin that has
// forgotten its entity tags can still answer the date.
func (validator Validator) revalidate(header http.Header) {
	if validator.ETag != "" {
		header.Set("If-None-Match", validator.ETag)
	}
	if date := validator.date(); date != "" {
		header.Set("If-Modified-Since", date)
	}
}

// precondition returns the header that ties one range request to the bytes the
// first response described, and reports whether there is one to send.
//
// A strong entity tag is preferred because it names the bytes outright. A
// modification time is the fallback, and it is weaker: an origin that changes a
// file twice inside one second reports the same time for both.
func (validator Validator) precondition() (string, string, bool) {
	if validator.Strong() {
		return "If-Match", validator.ETag, true
	}
	if date := validator.date(); date != "" {
		return "If-Unmodified-Since", date, true
	}
	return "", "", false
}

// date returns the Last-Modified value to send, or an empty string for none.
func (validator Validator) date() string {
	if validator.lastModifiedText != "" {
		return validator.lastModifiedText
	}
	if validator.LastModified.IsZero() {
		return ""
	}
	return validator.LastModified.UTC().Format(http.TimeFormat)
}
