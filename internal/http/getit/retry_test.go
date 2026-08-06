package getit

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestARetryableStatusIsTriedAgain(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(count int64) int {
		if count <= 2 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	getter := New()
	defer getter.Close()

	data, _ := fetch(t, getter, server.start(t))

	if !equal(data, server.data) {
		t.Fatal("the file did not arrive whole")
	}
	if got := server.requests.Load(); got != 3 {
		t.Errorf("the server saw %d requests, want the first try and two retries", got)
	}
}

func TestRetriesRunOut(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(int64) int { return http.StatusServiceUnavailable }
	getter := New()
	defer getter.Close()

	_, err := getter.Get(t.Context(), server.start(t), WithRetries(1))

	if err == nil {
		t.Fatal("a 503 that never went away was accepted")
	}
	if got := server.requests.Load(); got != 2 {
		t.Errorf("the server saw %d requests, want the first try and one retry", got)
	}
}

func TestAStatusNobodyWillAnswerIsNotTriedAgain(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(int64) int { return http.StatusNotFound }
	getter := New()
	defer getter.Close()

	if _, err := getter.Get(t.Context(), server.start(t)); err == nil {
		t.Fatal("a 404 was accepted")
	}

	if got := server.requests.Load(); got != 1 {
		t.Errorf("the server saw %d requests, want 1", got)
	}
}

// A decorator that refuses a request is not asked the same question again on
// every retry of every range.
func TestAPermanentRefusalIsMadeOnce(t *testing.T) {
	server := newFileServer(testChunk)
	refused := errors.New("this host is not allowed")
	attempts := 0
	getter := New(WithTransportDecorator(func(next http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			return nil, Permanent(refused)
		})
	}))
	defer getter.Close()

	_, err := getter.Get(t.Context(), server.start(t))

	if !errors.Is(err, refused) {
		t.Fatalf("error is %v, want the decorator's own error", err)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Error("the refusal does not match ErrPermanent")
	}
	if attempts != 1 {
		t.Errorf("the decorator was asked %d times, want 1", attempts)
	}
}

// The same refusal without Permanent is retried, which is what the wrapper
// exists to avoid.
func TestAPlainRefusalIsTriedAgain(t *testing.T) {
	server := newFileServer(testChunk)
	attempts := 0
	getter := New(WithTransportDecorator(func(next http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("this host is not allowed")
		})
	}))
	defer getter.Close()

	if _, err := getter.Get(t.Context(), server.start(t), WithRetries(2)); err == nil {
		t.Fatal("the refusal was accepted")
	}

	if attempts != 3 {
		t.Errorf("the decorator was asked %d times, want the first try and two retries", attempts)
	}
}

func TestARetryPolicyCanRefuseAStatusThisPackageWouldRetry(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(int64) int { return http.StatusServiceUnavailable }
	getter := New(WithRetryPolicy(func(err error) bool {
		var status *StatusError
		if errors.As(err, &status) && status.Status == http.StatusServiceUnavailable {
			return false
		}
		return Retryable(err)
	}))
	defer getter.Close()

	if _, err := getter.Get(t.Context(), server.start(t)); err == nil {
		t.Fatal("a 503 was accepted")
	}

	if got := server.requests.Load(); got != 1 {
		t.Errorf("the server saw %d requests, want 1", got)
	}
}

// The first decorator added is the outermost, so it is the first to see a
// request and the last to see a response.
func TestDecoratorsWrapInOrder(t *testing.T) {
	server := newFileServer(testChunk)
	var order []string
	mark := func(name string) TransportDecorator {
		return func(next http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				order = append(order, name)
				return next.RoundTrip(request)
			})
		}
	}
	getter := New(WithTransportDecorator(mark("outer"), mark("inner")))
	defer getter.Close()

	fetch(t, getter, server.start(t))

	if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
		t.Errorf("the decorators ran %v, want outer then inner", order)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "a network this package does not recognize", err: errors.New("connection reset"), want: true},
		{name: "too many requests", err: &StatusError{Status: http.StatusTooManyRequests}, want: true},
		{name: "a request timeout", err: &StatusError{Status: http.StatusRequestTimeout}, want: true},
		{name: "a server that is unwell", err: &StatusError{Status: http.StatusBadGateway}, want: true},
		{name: "not found", err: &StatusError{Status: http.StatusNotFound}},
		{name: "forbidden", err: &StatusError{Status: http.StatusForbidden}},
		{name: "a refusal", err: Permanent(errors.New("no"))},
		{name: "a URL the policy refused", err: ErrNotPermitted},
		{name: "a body over the limit", err: ErrTooLarge},
		{name: "a context that ended", err: context.Canceled},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := Retryable(test.err); got != test.want {
				t.Errorf("Retryable is %v, want %v", got, test.want)
			}
		})
	}
}

// An origin that names a wait gets the wait it named, up to what a caller with
// a deadline of its own is willing to give.
func TestBackoffHonoursRetryAfter(t *testing.T) {
	asked := backoff(1, &StatusError{Status: http.StatusTooManyRequests, RetryAfter: 5 * time.Second})
	if asked != 5*time.Second {
		t.Errorf("the wait is %s, want the 5s the origin asked for", asked)
	}

	capped := backoff(1, &StatusError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour})
	if capped != limitRetryAfter {
		t.Errorf("the wait is %s, want it capped at %s", capped, limitRetryAfter)
	}
}

// Without a wait to honour, each attempt waits longer than the one before, up
// to a limit, with a random part so that transfers that failed together do not
// come back all at once.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	err := errors.New("connection reset")
	first := backoff(1, err)
	if first < firstBackoff || first >= firstBackoff*2 {
		t.Errorf("the first wait is %s, want it just over %s", first, firstBackoff)
	}
	if backoff(20, err) > limitBackoff*2 {
		t.Errorf("a late wait is %s, want it capped near %s", backoff(20, err), limitBackoff)
	}
}

func TestRetryAfterReadsBothForms(t *testing.T) {
	seconds := &http.Response{Header: http.Header{"Retry-After": []string{"12"}}}
	if got := retryAfter(seconds); got != 12*time.Second {
		t.Errorf("a count of seconds read as %s, want 12s", got)
	}

	when := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	date := &http.Response{Header: http.Header{"Retry-After": []string{when}}}
	if got := retryAfter(date); got < 25*time.Second || got > 30*time.Second {
		t.Errorf("a date read as %s, want about 30s", got)
	}

	none := &http.Response{Header: http.Header{}}
	if got := retryAfter(none); got != 0 {
		t.Errorf("a missing header read as %s, want 0", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (transport roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
