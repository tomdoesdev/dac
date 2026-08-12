package asset

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/tomdoesdev/dac/internal/project"
)

// roundTripFunc makes transport outcomes deterministic without opening a
// network listener for error-classification tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestDownloaderPreservesTransportCause(t *testing.T) {
	cause := errors.New("test TLS cause")
	downloader := newTestDownloader(func(*http.Request) (*http.Response, error) { return nil, cause })
	request, err := http.NewRequest(http.MethodGet, "https://example.com/asset", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloader.doRequest(context.Background(), "asset", request)
	if !errors.Is(err, ErrDownloadTransport) || !errors.Is(err, cause) {
		t.Fatalf("doRequest error = %v, want transport class and original cause", err)
	}
}

func TestDownloaderEnforcesDeclaredAndStreamedMaximums(t *testing.T) {
	downloads, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = downloads.Close() }()

	reads := 0
	downloader := newTestDownloader(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, ContentLength: 4,
			Body: &countingBody{reader: strings.NewReader("data"), reads: &reads},
		}, nil
	})
	_, err = downloader.Download(context.Background(), downloads, Request{
		Name: "asset", URL: "https://example.com/asset", File: "asset", Policy: TransferPolicy{MaxSize: 3},
	}, "")
	if !errors.Is(err, ErrDownloadTooLarge) || reads != 0 {
		t.Fatalf("declared size error = %v, reads=%d; want early ErrDownloadTooLarge", err, reads)
	}

	request := Request{Name: "asset", File: "asset", Policy: TransferPolicy{MaxSize: 3}}
	if _, err := stageResponseWithPolicy(downloads, request, strings.NewReader("data"), "", -1, context.Background()); !errors.Is(err, ErrDownloadTooLarge) {
		t.Fatalf("streamed size error = %v, want ErrDownloadTooLarge", err)
	}
	download, err := stageResponseWithPolicy(downloads, request, strings.NewReader("abc"), "", -1, context.Background())
	if err != nil {
		t.Fatalf("exact maximum failed: %v", err)
	}
	_ = download.Discard()
}

func TestDownloaderClassifiesIdleBodyRead(t *testing.T) {
	downloads, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = downloads.Close() }()
	downloader := newTestDownloader(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: &contextBody{ctx: request.Context()}}, nil
	})
	_, err = downloader.Download(context.Background(), downloads, Request{
		Name: "asset", URL: "https://example.com/asset", File: "asset", Policy: TransferPolicy{IdleTimeout: 10 * time.Millisecond},
	}, "")
	if !errors.Is(err, ErrDownloadIdleTimeout) {
		t.Fatalf("Download error = %v, want ErrDownloadIdleTimeout", err)
	}
	var operation *project.Error
	if !errors.As(err, &operation) || operation.Kind() != project.ErrorKindNetwork {
		t.Fatalf("Download error kind = %v, want network", err)
	}
}

func TestCopyResponseIdentifiesDestinationFailure(t *testing.T) {
	cause := errors.New("disk full")
	_, _, writeErr, err := copyResponseWithDigest(failingWriter{err: cause}, strings.NewReader("data"), 0)
	if !errors.Is(err, cause) || !errors.Is(writeErr, cause) {
		t.Fatalf("copy errors = %v, %v; want destination cause", writeErr, err)
	}
}

type countingBody struct {
	reader io.Reader
	reads  *int
}

func (body *countingBody) Read(buffer []byte) (int, error) {
	*body.reads++
	return body.reader.Read(buffer)
}
func (*countingBody) Close() error { return nil }

type contextBody struct{ ctx context.Context }

func (body *contextBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}
func (*contextBody) Close() error { return nil }

type failingWriter struct{ err error }

func (writer failingWriter) Write([]byte) (int, error) { return 0, writer.err }

// TestDownloadErrorsAreClassifiable protects the sentinels exposed for each
// distinct download failure while project.Error adds user-facing context.
func TestDownloadErrorsAreClassifiable(t *testing.T) {
	downloads, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = downloads.Close() }()

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{
			name: "invalid request",
			run: func() error {
				_, err := newTestDownloader(nil).newRequest(context.Background(), Request{Name: "asset", URL: "http://[::1"})
				return err
			},
			want: ErrInvalidDownloadRequest,
		},
		{
			name: "transport failure",
			run: func() error {
				downloader := newTestDownloader(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("test transport failure")
				})
				request, err := http.NewRequest(http.MethodGet, "https://example.com/asset", nil)
				if err != nil {
					return err
				}
				_, err = downloader.doRequest(context.Background(), "asset", request)
				return err
			},
			want: ErrDownloadTransport,
		},
		{
			name: "unsuccessful status",
			run: func() error {
				downloader := newTestDownloader(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
				})
				request, err := http.NewRequest(http.MethodGet, "https://example.com/asset", nil)
				if err != nil {
					return err
				}
				_, err = downloader.doRequest(context.Background(), "asset", request)
				return err
			},
			want: ErrDownloadStatus,
		},
		{
			name: "response body failure",
			run: func() error {
				_, err := stageResponseWithPolicy(downloads, Request{Name: "asset", File: "artifact"}, iotest.ErrReader(errors.New("test read failure")), "", -1, context.Background())
				return err
			},
			want: ErrDownloadBody,
		},
		{
			name: "digest mismatch",
			run: func() error {
				return verifyExpectedDigest("asset", "sha256:"+strings.Repeat("0", 64), "sha256:"+strings.Repeat("1", 64))
			},
			want: ErrDigestMismatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want error matching %v", err, test.want)
			}
		})
	}
}

// newTestDownloader constructs the smallest client needed to exercise a
// transport outcome; a nil function is valid when no request will be sent.
func newTestDownloader(roundTrip roundTripFunc) *Downloader {
	return NewDownloader(&http.Client{Transport: roundTrip}, "dac/test")
}
