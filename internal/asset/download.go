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
	"github.com/tomdoesdev/dac/internal/redact"
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
	// chunkSize is the largest byte range requested at a time from a host that
	// supports ranges. It is a field rather than a constant only so tests can
	// drive the multi-chunk path without transferring megabytes.
	chunkSize int64
}

// NewDownloader constructs a downloader from an injectable client and the
// process-controlled user agent sent to remote hosts.
func NewDownloader(client *http.Client, userAgent string) *Downloader {
	return &Downloader{client: client, userAgent: userAgent, chunkSize: defaultChunkSize}
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
// expected digest before anything reaches the managed destination. The bytes
// arrive either as one response or as a sequence of byte ranges, which is a
// decision the remote host makes and the rest of this function does not see.
func (downloader *Downloader) Download(ctx context.Context, downloads *os.Root, request Request, expected string) (*StagedDownload, error) {
	requestContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	transfer, err := downloader.begin(requestContext, cancel, request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transfer.body.Close() }()
	return stageResponseWithPolicy(downloads, request, transfer.body, expected, transfer.size, requestContext)
}

// CalculateDigest retrieves an asset and returns the digest of the bytes the
// remote host served, without installing them or accepting them as trusted.
// This is dac's trust-on-first-use step: the caller records the digest as a
// limit that a later download must satisfy, so the bytes measured here are
// deliberately discarded rather than reused.
func (downloader *Downloader) CalculateDigest(ctx context.Context, downloads *os.Root, request Request) (string, error) {
	download, err := downloader.Download(ctx, downloads, request, "")
	if err != nil {
		return "", err
	}
	digest := download.Digest
	if err := download.Discard(); err != nil {
		return "", fault.NewFilesystemError(err)
	}
	return digest, nil
}

// newRequest builds the request for the URL the manifest named.
func (downloader *Downloader) newRequest(ctx context.Context, request Request) (*http.Request, error) {
	return downloader.newRequestTo(ctx, request, request.URL, true)
}

// newRequestTo builds a request for one location, which is not always the URL
// the manifest named once a redirect has been followed. Configured headers are
// resolved at the last possible moment so environment-backed secrets exist only
// for the lifetime of the HTTP request, and the caller decides whether they may
// be sent at all, because a location outside the manifest's origin must not
// receive them.
func (downloader *Downloader) newRequestTo(ctx context.Context, request Request, location string, configured bool) (*http.Request, error) {
	headers := make(http.Header)
	if configured {
		resolved, err := requestHeaders(request.Headers)
		if err != nil {
			return nil, fault.NewConfigurationError(err, fault.WithAsset(request.Name))
		}
		headers = resolved
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, fault.NewConfigurationError(ErrInvalidDownloadRequest, fault.WithAsset(request.Name))
	}
	httpRequest.Header = headers
	httpRequest.Header.Set("User-Agent", downloader.userAgent)
	return httpRequest, nil
}

// doRequest is the whole-response form of a request: a status outside HTTP's
// successful range is rejected before its body can be mistaken for a
// downloaded artifact.
func (downloader *Downloader) doRequest(ctx context.Context, assetName string, request *http.Request) (*http.Response, error) {
	response, err := downloader.send(ctx, assetName, request)
	if err != nil {
		return nil, err
	}
	if err := rejectUnsuccessfulStatus(assetName, response); err != nil {
		return nil, err
	}
	return response, nil
}

// send owns transport-specific failure classification. It leaves the status
// alone for callers that treat one unsuccessful code as an answer rather than
// a failure, such as the range probe and its 416.
func (downloader *Downloader) send(ctx context.Context, assetName string, request *http.Request) (*http.Response, error) {
	response, err := downloader.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fault.NewCancelledError(context.Cause(ctx), fault.WithAsset(assetName))
		}
		// net/http quotes the URL it failed on, which for a dac asset can carry
		// a token in its query string. Redact as the transport error enters
		// dac's chain rather than trusting the render boundary to catch it.
		return nil, fault.NewNetworkError(fmt.Errorf("%w: %w", ErrDownloadTransport, redact.Error(err)), fault.WithAsset(assetName))
	}
	return response, nil
}

// rejectUnsuccessfulStatus closes and classifies an error response.
func rejectUnsuccessfulStatus(assetName string, response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	_ = response.Body.Close()
	return fault.NewNetworkError(&downloadStatusError{statusCode: response.StatusCode}, fault.WithAsset(assetName))
}

// stageResponseWithPolicy separates remote read failures from local write
// failures and applies transfer policy before returning verified staged bytes.
// The declared size is whatever the host committed to up front — a
// Content-Length, or the complete length behind a byte range — so an asset too
// large to accept is refused before any of it is transferred.
func stageResponseWithPolicy(downloads *os.Root, request Request, body io.Reader, expected string, declaredSize int64, ctx context.Context) (_ *StagedDownload, err error) {
	if err := verifyMaximumSize(request.Name, request.Policy.MaxSize, declaredSize, false); err != nil {
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
		// A chunked transfer requests bytes while the copy runs, so a failure
		// arriving here may already carry its own classification. Wrapping it
		// again would describe a failed request as a failed body read.
		var operation *fault.Error
		if errors.As(err, &operation) {
			return nil, err
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

// copyBufferSize is how much of an artifact moves per read, hash, and write.
// io.Copy's own 32 KiB would spend an order of magnitude more system calls and
// hash invocations on the multi-gigabyte artifacts dac exists to fetch.
const copyBufferSize = 512 << 10

// copyResponseWithDigest records which side of the copy failed while retaining
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
	// Neither side implements ReaderFrom or WriterTo here, so the buffer is
	// the one that is actually used rather than a hint io.CopyBuffer ignores.
	size, err := io.CopyBuffer(io.MultiWriter(trackedDestination, hasher), reader, make([]byte, copyBufferSize))
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

// guardIdleReads applies the policy's idle timeout to one response body, and
// returns the body unchanged when the policy disables the limit.
func guardIdleReads(ctx context.Context, cancel context.CancelCauseFunc, policy TransferPolicy, body io.ReadCloser) io.ReadCloser {
	if policy.IdleTimeout <= 0 {
		return body
	}
	return &idleReadCloser{body: body, timeout: policy.IdleTimeout, ctx: ctx, cancel: cancel}
}

// idleReadCloser cancels the request only while a body read is blocked. Time
// spent hashing or writing local bytes therefore cannot trigger network idle.
type idleReadCloser struct {
	body    io.ReadCloser
	timeout time.Duration
	ctx     context.Context
	cancel  context.CancelCauseFunc
	// timer is reused across reads because a large artifact performs thousands
	// of them, and each one would otherwise allocate a timer of its own.
	timer *time.Timer
}

func (reader *idleReadCloser) Read(buffer []byte) (int, error) {
	if reader.timer == nil {
		reader.timer = time.AfterFunc(reader.timeout, func() { reader.cancel(ErrDownloadIdleTimeout) })
	} else {
		reader.timer.Reset(reader.timeout)
	}
	count, err := reader.body.Read(buffer)
	reader.timer.Stop()
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
