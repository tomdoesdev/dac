package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/atomic"
)

// Request is the concrete, validated information needed to retrieve an asset.
// Keeping it independent of manifest types prevents transport from owning
// configuration or resolution policy.
type Request struct {
	Name    string
	URL     string
	File    string
	Headers map[string]string
}

var (
	// ErrInvalidDownloadRequest marks a URL that cannot form an HTTP request.
	ErrInvalidDownloadRequest = errors.New("cannot create HTTP request")
	// ErrDownloadTransport marks a request that failed before a response arrived.
	ErrDownloadTransport = errors.New("failed to download from remote host")
	// ErrDownloadStatus marks a response outside HTTP's successful status range.
	ErrDownloadStatus = errors.New("download returned an unsuccessful HTTP status")
	// ErrDownloadBody marks a failure while streaming response bytes.
	ErrDownloadBody = errors.New("failed while downloading response body")
	// ErrDigestMismatch marks downloaded bytes that fail integrity verification.
	ErrDigestMismatch = errors.New("downloaded bytes do not match expected digest")
)

// downloadStatusError preserves the response code in human-readable output
// while allowing callers to classify every non-success status with errors.Is.
type downloadStatusError struct {
	statusCode int
}

func (err *downloadStatusError) Error() string {
	return fmt.Sprintf("server returned HTTP %d", err.statusCode)
}

func (err *downloadStatusError) Unwrap() error {
	return ErrDownloadStatus
}

// Downloader binds the process's HTTP policy and identity for every request.
type Downloader struct {
	client    *http.Client
	userAgent string
}

// NewDownloader constructs a downloader from an injectable client and the
// process-controlled user agent sent to remote hosts.
func NewDownloader(client *http.Client, userAgent string) *Downloader {
	return &Downloader{client: client, userAgent: userAgent}
}

// StagedDownload is verified bytes in a destination-adjacent temporary file.
// Callers choose the commit mode that matches their transaction semantics.
type StagedDownload struct {
	file   *atomic.File
	Digest string
	Size   int64
}

func (download *StagedDownload) Discard() error { return download.file.Discard() }
func (download *StagedDownload) Commit() error  { return download.file.Commit() }
func (download *StagedDownload) CommitReversible() (*atomic.Commit, error) {
	return download.file.CommitReversible()
}

// Download streams one artifact into a temporary file and verifies an optional
// expected digest before anything reaches the managed destination.
func (downloader *Downloader) Download(ctx context.Context, downloads *os.Root, request Request, expected string) (*StagedDownload, error) {
	httpRequest, err := downloader.newRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	response, err := downloader.doRequest(ctx, request.Name, httpRequest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	return stageResponse(downloads, request, response.Body, expected)
}

// newRequest resolves configured headers at the last possible moment so
// environment-backed secrets exist only for the lifetime of the HTTP request.
func (downloader *Downloader) newRequest(ctx context.Context, request Request) (*http.Request, error) {
	headers, err := requestHeaders(request.Headers)
	if err != nil {
		return nil, project.NewConfigurationError(err, project.WithAsset(request.Name))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return nil, project.NewConfigurationError(ErrInvalidDownloadRequest, project.WithAsset(request.Name))
	}
	httpRequest.Header = headers
	httpRequest.Header.Set("User-Agent", downloader.userAgent)
	return httpRequest, nil
}

// doRequest owns transport-specific failure classification and rejects error
// responses before their bodies can be mistaken for downloaded artifacts.
func (downloader *Downloader) doRequest(ctx context.Context, assetName string, request *http.Request) (*http.Response, error) {
	response, err := downloader.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, project.NewCancelledError(context.Canceled, project.WithAsset(assetName))
		}
		return nil, project.NewNetworkError(ErrDownloadTransport, project.WithAsset(assetName))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, project.NewNetworkError(&downloadStatusError{statusCode: response.StatusCode}, project.WithAsset(assetName))
	}
	return response, nil
}

// stageResponse streams and hashes a successful response into an atomic file.
// Any failure discards partial bytes before returning the original error.
func stageResponse(downloads *os.Root, request Request, body io.Reader, expected string) (_ *StagedDownload, err error) {
	temporary, err := atomic.CreateRoot(downloads, request.File, 0o644)
	if err != nil {
		return nil, project.NewFilesystemError(err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, project.NewFilesystemError(temporary.Discard()))
		}
	}()

	size, digest, err := copyWithDigest(temporary, body)
	if err != nil {
		return nil, project.NewNetworkError(ErrDownloadBody, project.WithAsset(request.Name))
	}
	if err := verifyExpectedDigest(request.Name, expected, digest); err != nil {
		return nil, err
	}
	return &StagedDownload{file: temporary, Digest: digest, Size: size}, nil
}

// verifyExpectedDigest keeps integrity policy separate from byte transfer and
// reports canonical digest values when an expectation is present.
func verifyExpectedDigest(assetName, expected, received string) error {
	if expected == "" {
		return nil
	}
	normalized, _ := NormalizeDigest(expected)
	if received == normalized {
		return nil
	}
	return project.NewIntegrityError(
		ErrDigestMismatch,
		project.WithAsset(assetName),
		project.WithIntegrity(normalized, received),
	)
}

// VerifyLocal hashes a regular managed file only when its size can match. A
// symlink is intentionally untrusted: later replacement must not follow it.
func VerifyLocal(downloads *os.Root, name, expected string, size int64) (bool, error) {
	info, err := downloads.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, project.NewFilesystemError(err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return false, nil
	}
	file, err := downloads.Open(name)
	if err != nil {
		return false, project.NewFilesystemError(err)
	}
	defer func() { _ = file.Close() }()
	digest, err := computeDigest(file)
	if err != nil {
		return false, project.NewFilesystemError(err)
	}
	return digest == expected, nil
}
