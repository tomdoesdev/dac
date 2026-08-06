package getit

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestAValidatorAsksWhetherTheBytesChanged(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New()
	defer getter.Close()

	response, err := getter.Get(t.Context(), server.start(t), WithValidator(Validator{ETag: `"v1"`}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if !response.NotModified {
		t.Fatalf("status is %d, want a 304", response.Status)
	}
	if got := server.seen("If-None-Match")[0]; got != `"v1"` {
		t.Errorf(`If-None-Match is %q, want "v1"`, got)
	}
	if response.Length != 0 {
		t.Errorf("length is %d, want 0 for a body that is not there", response.Length)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the empty body: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("a not-modified response carried %d bytes, want none", len(data))
	}
}

func TestAStaleValidatorGetsTheBody(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New()
	defer getter.Close()

	data, response := fetch(t, getter, server.start(t), WithValidator(Validator{ETag: `"v0"`}))

	if response.NotModified {
		t.Fatal("an entity tag the origin does not hold was answered with a 304")
	}
	if !equal(data, server.data) {
		t.Error("the body did not arrive whole")
	}
	if response.Validator.ETag != `"v1"` {
		t.Errorf(`the response reports the etag %q, want "v1"`, response.Validator.ETag)
	}
}

func TestAModificationDateRevalidates(t *testing.T) {
	server := newFileServer(testChunk)
	server.etag = ""
	server.lastModified = lastModified
	getter := New()
	defer getter.Close()
	target := server.start(t)

	// The first request has nothing to ask with, so it takes the body and the
	// date that came with it.
	_, first := fetch(t, getter, target)
	if first.Validator.LastModified.IsZero() {
		t.Fatal("the response reports no modification time")
	}

	// The second asks with what the first was told.
	response, err := getter.Get(t.Context(), target, WithValidator(first.Validator))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if !response.NotModified {
		t.Fatalf("status is %d, want a 304", response.Status)
	}
	// The date that goes back out is the one the origin wrote, not a
	// reformatting of it, because an origin that compares the text would
	// otherwise answer wrongly.
	if got := server.seen("If-Modified-Since")[1]; got != lastModified {
		t.Errorf("If-Modified-Since is %q, want %q", got, lastModified)
	}
}

func TestBothPreconditionsGoOutTogether(t *testing.T) {
	server := newFileServer(testChunk)
	server.lastModified = lastModified
	getter := New()
	defer getter.Close()

	when, err := http.ParseTime(lastModified)
	if err != nil {
		t.Fatalf("parsing the test date: %v", err)
	}
	response, getErr := getter.Get(t.Context(), server.start(t),
		WithValidator(Validator{ETag: `"v1"`, LastModified: when}))
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	defer func() { _ = response.Body.Close() }()

	if got := server.seen("If-None-Match")[0]; got != `"v1"` {
		t.Errorf(`If-None-Match is %q, want "v1"`, got)
	}
	// An origin that has forgotten its entity tags can still answer the date.
	if got := server.seen("If-Modified-Since")[0]; got != lastModified {
		t.Errorf("If-Modified-Since is %q, want %q", got, lastModified)
	}
}

// A validator a caller built by hand has no header text to echo, so the time is
// written in the form HTTP asks a sender to write.
func TestAValidatorBuiltByHandFormatsItsDate(t *testing.T) {
	when := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)
	validator := Validator{LastModified: when}

	if got := validator.date(); got != lastModified {
		t.Errorf("the date is %q, want %q", got, lastModified)
	}
}

func TestValidatorStrength(t *testing.T) {
	cases := []struct {
		name      string
		validator Validator
		empty     bool
		strong    bool
	}{
		{name: "nothing", validator: Validator{}, empty: true},
		{name: "a strong tag", validator: Validator{ETag: `"v1"`}, strong: true},
		{name: "a weak tag", validator: Validator{ETag: `W/"v1"`}},
		{name: "a date", validator: Validator{LastModified: time.Now()}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := test.validator.Empty(); got != test.empty {
				t.Errorf("Empty is %v, want %v", got, test.empty)
			}
			if got := test.validator.Strong(); got != test.strong {
				t.Errorf("Strong is %v, want %v", got, test.strong)
			}
		})
	}
}

func TestPreconditionPrefersTheEntityTag(t *testing.T) {
	when := time.Date(2015, time.October, 21, 7, 28, 0, 0, time.UTC)
	cases := []struct {
		name      string
		validator Validator
		wantName  string
		wantOK    bool
	}{
		{
			name:      "a strong tag names the bytes outright",
			validator: Validator{ETag: `"v1"`, LastModified: when},
			wantName:  "If-Match",
			wantOK:    true,
		},
		{
			name:      "a date is the fallback",
			validator: Validator{LastModified: when},
			wantName:  "If-Unmodified-Since",
			wantOK:    true,
		},
		{
			// A weak tag says two responses are equivalent, not that they are the
			// same bytes, which is the only question a byte range asks.
			name:      "a weak tag is not enough",
			validator: Validator{ETag: `W/"v1"`},
		},
		{name: "nothing is not enough", validator: Validator{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			name, _, ok := test.validator.precondition()
			if ok != test.wantOK {
				t.Fatalf("precondition reports %v, want %v", ok, test.wantOK)
			}
			if name != test.wantName {
				t.Errorf("the header is %q, want %q", name, test.wantName)
			}
		})
	}
}

// An unreadable date is a date this package can neither compare nor send back.
func TestAnUnreadableDateIsDropped(t *testing.T) {
	header := http.Header{}
	header.Set("Last-Modified", "the day before yesterday")
	header.Set("ETag", `"v1"`)

	validator := validatorFrom(header)

	if !validator.LastModified.IsZero() {
		t.Errorf("the date is %v, want the zero time", validator.LastModified)
	}
	if validator.date() != "" {
		t.Errorf("the date to send is %q, want nothing", validator.date())
	}
	if validator.ETag != `"v1"` {
		t.Errorf("the etag is %q, want the one the header carried", validator.ETag)
	}
}
