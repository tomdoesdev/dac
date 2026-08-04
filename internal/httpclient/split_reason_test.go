package httpclient

import (
	"net/http"
	"testing"
)

// TestSplittableNamesWhyItRefused covers the reason a split download did not
// happen, which is the thing an operator watching a large asset arrive over one
// connection actually needs.
//
// Every condition here is the origin's answer rather than a setting DAC got
// wrong, so naming which one turns "it was slow" into something to take to
// whoever runs the mirror.
func TestSplittableNamesWhyItRefused(t *testing.T) {
	strong := headers("Accept-Ranges", "bytes", "ETag", `"v1"`)
	cases := []struct {
		name        string
		parallelism int
		response    *http.Response
		want        string
	}{
		{
			name:        "one part",
			parallelism: 1,
			response:    &http.Response{StatusCode: 200, ContentLength: 64 << 20, Header: strong},
			want:        "download-parts is 1",
		},
		{
			name:        "not a 200",
			parallelism: 4,
			response:    &http.Response{StatusCode: 304, ContentLength: 64 << 20, Header: strong},
			want:        "response is not 200",
		},
		{
			// A chunked response has no length to divide into ranges, and this
			// is the case that used to look identical to an origin refusing.
			name:        "no length",
			parallelism: 4,
			response:    &http.Response{StatusCode: 200, ContentLength: -1, Header: strong},
			want:        "response has no length",
		},
		{
			name:        "too small",
			parallelism: 4,
			response:    &http.Response{StatusCode: 200, ContentLength: 1024, Header: strong},
			want:        "response is below the split size",
		},
		{
			name:        "no ranges",
			parallelism: 4,
			response:    &http.Response{StatusCode: 200, ContentLength: 64 << 20, Header: headers("ETag", `"v1"`)},
			want:        "origin does not serve byte ranges",
		},
		{
			// A weak ETag says two responses are equivalent, not that they are
			// the same bytes, which is the only question a byte range asks.
			name:        "weak validator only",
			parallelism: 4,
			response: &http.Response{StatusCode: 200, ContentLength: 64 << 20,
				Header: headers("Accept-Ranges", "bytes", "ETag", `W/"v1"`)},
			want: "origin sent no strong validator",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, refused := splittable(test.parallelism, test.response); refused != test.want {
				t.Fatalf("refused with %q, want %q", refused, test.want)
			}
		})
	}
}

// TestSplittableAcceptsAStrongValidator covers the two preconditions a split
// can be built on, and that neither reports a refusal.
func TestSplittableAcceptsAStrongValidator(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"strong etag", headers("Accept-Ranges", "bytes", "ETag", `"v1"`), "If-Match"},
		{"last modified", headers("Accept-Ranges", "bytes", "Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT"), "If-Unmodified-Since"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: 200, ContentLength: 64 << 20, Header: test.header}
			precondition, refused := splittable(4, response)
			if refused != "" {
				t.Fatalf("refused a splittable response: %q", refused)
			}
			if precondition.name != test.want {
				t.Fatalf("precondition is %q, want %q", precondition.name, test.want)
			}
		})
	}
}

// headers builds a response header from name and value pairs.
//
// It goes through Set rather than a map literal so the keys are canonicalized
// the way net/http canonicalizes what it parses off the wire. A literal
// "ETag" key is stored under a name Get never looks up -- Go canonicalizes it
// to "Etag" -- which makes a header the code cannot see look exactly like one
// the origin never sent.
func headers(pairs ...string) http.Header {
	header := http.Header{}
	for index := 0; index+1 < len(pairs); index += 2 {
		header.Set(pairs[index], pairs[index+1])
	}
	return header
}
