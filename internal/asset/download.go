package asset

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/tomdoesdev/dac/internal/fault"
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
	Policy  TransferPolicy
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
	// ErrDownloadTooLarge marks response bytes outside the configured policy.
	ErrDownloadTooLarge = errors.New("download exceeds configured maximum size")
	// ErrDownloadIdleTimeout marks a body read that made no progress in time.
	ErrDownloadIdleTimeout = errors.New("download response body timed out while idle")
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
	requestContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	httpRequest, err := downloader.newRequest(requestContext, request)
	if err != nil {
		return nil, err
	}
	response, err := downloader.doRequest(requestContext, request.Name, httpRequest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	body := io.ReadCloser(response.Body)
	if request.Policy.IdleTimeout > 0 {
		body = &idleReadCloser{body: body, timeout: request.Policy.IdleTimeout, ctx: requestContext, cancel: cancel}
	}
	return stageResponseWithPolicy(downloads, request, body, expected, response.ContentLength, requestContext)
}

// newRequest resolves configured headers at the last possible moment so
// environment-backed secrets exist only for the lifetime of the HTTP request.
func (downloader *Downloader) newRequest(ctx context.Context, request Request) (*http.Request, error) {
	headers, err := requestHeaders(request.Headers)
	if err != nil {
		return nil, fault.NewConfigurationError(err, fault.WithAsset(request.Name))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return nil, fault.NewConfigurationError(ErrInvalidDownloadRequest, fault.WithAsset(request.Name))
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
		if ctx.Err() != nil {
			return nil, fault.NewCancelledError(context.Cause(ctx), fault.WithAsset(assetName))
		}
		return nil, fault.NewNetworkError(fmt.Errorf("%w: %w", ErrDownloadTransport, err), fault.WithAsset(assetName))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fault.NewNetworkError(&downloadStatusError{statusCode: response.StatusCode}, fault.WithAsset(assetName))
	}
	return response, nil
}

// stageResponseWithPolicy separates remote read failures from local write
// failures and applies transfer policy before returning verified staged bytes.
func stageResponseWithPolicy(downloads *os.Root, request Request, body io.Reader, expected string, contentLength int64, ctx context.Context) (_ *StagedDownload, err error) {
	if err := verifyMaximumSize(request.Name, request.Policy.MaxSize, contentLength, false); err != nil {
		return nil, err
	}
	temporary, err := atomic.CreateRoot(downloads, request.File, 0o644)
	if err != nil {
		return nil, fault.NewFilesystemError(err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, fault.NewFilesystemError(temporary.Discard()))
		}
	}()

	size, digest, writeErr, err := copyResponseWithDigest(temporary, body, request.Policy.MaxSize)
	if err != nil {
		if writeErr != nil {
			return nil, fault.NewFilesystemError(writeErr, fault.WithAsset(request.Name))
		}
		if errors.Is(context.Cause(ctx), ErrDownloadIdleTimeout) || errors.Is(err, ErrDownloadIdleTimeout) {
			return nil, fault.NewNetworkError(ErrDownloadIdleTimeout, fault.WithAsset(request.Name))
		}
		if ctx.Err() != nil {
			return nil, fault.NewCancelledError(context.Cause(ctx), fault.WithAsset(request.Name))
		}
		return nil, fault.NewNetworkError(fmt.Errorf("%w: %w", ErrDownloadBody, err), fault.WithAsset(request.Name))
	}
	if err := verifyMaximumSize(request.Name, request.Policy.MaxSize, size, true); err != nil {
		return nil, err
	}
	if err := verifyExpectedDigest(request.Name, expected, digest); err != nil {
		return nil, err
	}
	return &StagedDownload{file: temporary, Digest: digest, Size: size}, nil
}

// copyResponseWithDigest records which side of io.Copy failed while retaining
// the single-pass hashing behavior used for large artifacts.
func copyResponseWithDigest(destination io.Writer, source io.Reader, maximum int64) (int64, string, error, error) {
	trackedDestination := &recordingWriter{writer: destination}
	reader := source
	if maximum > 0 {
		limit := maximum
		if maximum < math.MaxInt64 {
			limit++
		}
		reader = &io.LimitedReader{R: source, N: limit}
	}
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(trackedDestination, hasher), reader)
	if err != nil {
		return size, "", trackedDestination.err, err
	}
	return size, formatSHA256Digest(hasher.Sum(nil)), nil, nil
}

// verifyMaximumSize handles both authoritative Content-Length values and the
// max+1 observation used when a streamed response has no reliable length.
func verifyMaximumSize(assetName string, maximum, received int64, streamed bool) error {
	if maximum <= 0 || received < 0 || received <= maximum {
		return nil
	}
	receivedValue := strconv.FormatInt(received, 10)
	if streamed {
		receivedValue = "at least " + receivedValue
	}
	return fault.NewIntegrityError(
		ErrDownloadTooLarge,
		fault.WithAsset(assetName),
		fault.WithIntegrity("at most "+strconv.FormatInt(maximum, 10)+" bytes", receivedValue+" bytes"),
	)
}

type recordingWriter struct {
	writer io.Writer
	err    error
}

// Write remembers destination-side failures so io.MultiWriter's single error
// can still be classified as a local filesystem problem by the caller.
func (writer *recordingWriter) Write(buffer []byte) (int, error) {
	count, err := writer.writer.Write(buffer)
	if err == nil && count != len(buffer) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.err = err
	}
	return count, err
}

// idleReadCloser cancels the request only while a body read is blocked. Time
// spent hashing or writing local bytes therefore cannot trigger network idle.
type idleReadCloser struct {
	body    io.ReadCloser
	timeout time.Duration
	ctx     context.Context
	cancel  context.CancelCauseFunc
}

func (reader *idleReadCloser) Read(buffer []byte) (int, error) {
	timer := time.AfterFunc(reader.timeout, func() { reader.cancel(ErrDownloadIdleTimeout) })
	count, err := reader.body.Read(buffer)
	timer.Stop()
	if errors.Is(context.Cause(reader.ctx), ErrDownloadIdleTimeout) {
		return count, ErrDownloadIdleTimeout
	}
	return count, err
}

func (reader *idleReadCloser) Close() error { return reader.body.Close() }

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
	return fault.NewIntegrityError(
		ErrDigestMismatch,
		fault.WithAsset(assetName),
		fault.WithIntegrity(normalized, received),
	)
}

// VerifyLocal hashes a regular managed file only when its size can match. A
// symlink is intentionally untrusted: later replacement must not follow it.
func VerifyLocal(downloads *os.Root, name, expected string, size int64) (bool, error) {
	inspection, err := InspectLocal(downloads, name, expected, size)
	return inspection.State == LocalVerified, err
}

// LocalState describes the on-disk side of one current lock entry.
type LocalState string

const (
	LocalMissing  LocalState = "missing"
	LocalInvalid  LocalState = "invalid"
	LocalVerified LocalState = "verified"
)

// LocalInspection distinguishes absence from invalid bytes while sharing the
// exact verification path used by pull and status.
type LocalInspection struct {
	State  LocalState
	Reason string
}

// InspectLocal hashes a regular managed file only when its size can match. A
// symlink is intentionally invalid because later replacement must not follow it.
func InspectLocal(downloads *os.Root, name, expected string, size int64) (LocalInspection, error) {
	info, err := downloads.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return LocalInspection{State: LocalMissing, Reason: "download is missing"}, nil
	}
	if err != nil {
		return LocalInspection{}, fault.NewFilesystemError(err)
	}
	if !info.Mode().IsRegular() {
		return LocalInspection{State: LocalInvalid, Reason: "download is not a regular file"}, nil
	}
	if info.Size() != size {
		return LocalInspection{State: LocalInvalid, Reason: "download size does not match dac.lock"}, nil
	}
	file, err := downloads.Open(name)
	if err != nil {
		return LocalInspection{}, fault.NewFilesystemError(err)
	}
	defer func() { _ = file.Close() }()
	digest, err := computeDigest(file)
	if err != nil {
		return LocalInspection{}, fault.NewFilesystemError(err)
	}
	if digest != expected {
		return LocalInspection{State: LocalInvalid, Reason: "download digest does not match dac.lock"}, nil
	}
	return LocalInspection{State: LocalVerified}, nil
}
