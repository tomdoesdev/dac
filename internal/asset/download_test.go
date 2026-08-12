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
)

// roundTripFunc makes transport outcomes deterministic without opening a
// network listener for error-classification tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

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
				_, err := stageResponse(downloads, Request{Name: "asset", File: "artifact"}, iotest.ErrReader(errors.New("test read failure")), "")
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
