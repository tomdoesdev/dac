package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tomdoesdev/dac/internal/fault"
)

const (
	// defaultChunkSize is the largest byte range dac asks for at a time. One
	// request per chunk costs a round trip that a single streamed response
	// would not, so the chunk is large enough for that cost to disappear into
	// the transfer even on a fast link, while still small enough that a failed
	// chunk is cheap to fetch again.
	defaultChunkSize int64 = 16 << 20
	// chunkResumeBudget bounds how many times one download may continue from
	// the last byte it accepted. Resuming is what makes chunking worth its
	// extra requests; an unbounded budget would turn a host that fails
	// immediately into an infinite loop.
	chunkResumeBudget = 4

	rangeHeader        = "Range"
	ifRangeHeader      = "If-Range"
	contentRangeHeader = "Content-Range"
	contentRangeUnit   = "bytes "
)

// ErrDownloadRangeMismatch marks a host that stopped honouring the byte ranges
// dac asked for part-way through a transfer.
var ErrDownloadRangeMismatch = errors.New("remote host did not serve the requested byte range")

// errUnparsableContentRange is internal: every caller either falls back to a
// single stream or reports the mismatch with the range dac asked for.
var errUnparsableContentRange = errors.New("unparsable Content-Range")

// rangeError preserves what dac asked for and what arrived while allowing
// callers to classify every range failure with errors.Is.
type rangeError struct{ detail string }

func (err *rangeError) Error() string { return err.detail }

func (err *rangeError) Unwrap() error { return ErrDownloadRangeMismatch }

func newRangeError(detail string) error { return &rangeError{detail: detail} }

// source is one asset's byte stream together with the complete size the remote
// host declared for it, which is negative when it declared none.
type source struct {
	body io.ReadCloser
	size int64
}

// begin issues the first request as a byte range, so a single round trip both
// starts the transfer and reveals whether the host can serve the rest of the
// asset in chunks. A host that ignores Range answers 200 with the whole body,
// which dac streams exactly as it did before ranges existed, so range support
// costs nothing when it is absent.
func (downloader *Downloader) begin(ctx context.Context, cancel context.CancelCauseFunc, request Request) (source, error) {
	if downloader.chunkSize <= 0 {
		return downloader.stream(ctx, cancel, request)
	}
	httpRequest, err := downloader.newRequest(ctx, request)
	if err != nil {
		return source{}, err
	}
	httpRequest.Header.Set(rangeHeader, byteRange(0, downloader.chunkSize-1))
	response, err := downloader.send(ctx, request.Name, httpRequest)
	if err != nil {
		return source{}, err
	}
	// A host that cannot satisfy the probe at all, which an empty asset always
	// answers with, still serves that asset whole. Spending one extra round
	// trip there is better than failing a download plain GET would complete.
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		_ = response.Body.Close()
		return downloader.stream(ctx, cancel, request)
	}
	if err := rejectUnsuccessfulStatus(request.Name, response); err != nil {
		return source{}, err
	}
	if response.StatusCode != http.StatusPartialContent {
		return source{body: guardIdleReads(ctx, cancel, request.Policy, response.Body), size: response.ContentLength}, nil
	}
	first, _, complete, err := parseContentRange(response.Header.Get(contentRangeHeader))
	if err != nil || first != 0 {
		// A 206 dac cannot account for byte-exactly is not worth guessing at,
		// because every later chunk is placed relative to this one.
		_ = response.Body.Close()
		return downloader.stream(ctx, cancel, request)
	}
	reader := downloader.newChunkReader(ctx, cancel, request, httpRequest.URL, response, complete)
	return source{body: reader, size: complete}, nil
}

// stream fetches the whole asset in one response, which is the path for hosts
// that do not support byte ranges.
func (downloader *Downloader) stream(ctx context.Context, cancel context.CancelCauseFunc, request Request) (source, error) {
	httpRequest, err := downloader.newRequest(ctx, request)
	if err != nil {
		return source{}, err
	}
	response, err := downloader.doRequest(ctx, request.Name, httpRequest)
	if err != nil {
		return source{}, err
	}
	return source{body: guardIdleReads(ctx, cancel, request.Policy, response.Body), size: response.ContentLength}, nil
}

// chunkReader presents the remaining bytes of a range-capable asset as one
// ordered stream. Each chunk is a separate request on the same kept-alive
// connection, so the bytes still arrive strictly in order and only one is in
// flight, but a failure now costs the current chunk instead of the whole
// transfer: the next request resumes at the first byte dac has not accepted.
type chunkReader struct {
	downloader *Downloader
	ctx        context.Context
	cancel     context.CancelCauseFunc
	request    Request
	// target is where the remaining chunks are fetched from. The first
	// response may have been redirected, and walking that chain again for
	// every chunk would spend a round trip each time.
	target string
	// configured records whether the manifest's headers may be sent to target.
	// It follows redirectPolicy's rule so that a redirect leading away from
	// the origin the manifest named cannot be turned into a credential leak by
	// requesting the later chunks directly.
	configured bool
	// validator is the If-Range value that ties every chunk to the version of
	// the asset the first response served, when the host offered one.
	validator string
	// total is the complete size the host declared, and offset is the first
	// byte dac has not yet accepted.
	total  int64
	offset int64
	// served counts the bytes the current chunk has delivered, so a response
	// that carries nothing can be told apart from one that ended early.
	served  int64
	body    io.ReadCloser
	resumes int
}

// newChunkReader adopts the probe response as the first chunk, so the request
// that discovered range support also carries its bytes.
func (downloader *Downloader) newChunkReader(ctx context.Context, cancel context.CancelCauseFunc, request Request, requested *url.URL, response *http.Response, total int64) *chunkReader {
	final := finalURL(requested, response)
	reader := &chunkReader{
		downloader: downloader,
		ctx:        ctx,
		cancel:     cancel,
		request:    request,
		target:     final.String(),
		configured: sameOrigin(requested, final),
		validator:  rangeValidator(response.Header),
		total:      total,
		resumes:    chunkResumeBudget,
	}
	reader.adopt(response)
	return reader
}

// Read returns the next bytes of the asset, requesting the next chunk when the
// current one runs out. A resumable failure never reaches the caller: it costs
// a request rather than the bytes already hashed.
func (reader *chunkReader) Read(buffer []byte) (int, error) {
	for {
		if reader.body == nil {
			if reader.offset >= reader.total {
				return 0, io.EOF
			}
			if err := reader.open(); err != nil {
				if !reader.resume(err) {
					return 0, err
				}
				continue
			}
		}
		count, err := reader.body.Read(buffer)
		reader.offset += int64(count)
		reader.served += int64(count)
		if reader.offset > reader.total {
			return count, newRangeError(fmt.Sprintf("host served more than the %d bytes it declared", reader.total))
		}
		if err == nil {
			return count, nil
		}
		_ = reader.closeChunk()
		switch {
		case !errors.Is(err, io.EOF):
			if !reader.resume(err) {
				return count, err
			}
		case reader.offset >= reader.total:
			return count, io.EOF
		case reader.served == 0:
			// The next request would ask for exactly the bytes this one failed
			// to deliver, so continuing could never make progress.
			return count, newRangeError(fmt.Sprintf("host served no bytes for the range starting at %d", reader.offset))
		}
		// A chunk that ended early is resumed from wherever it stopped, which
		// is the same work as moving on to the next chunk.
		if count > 0 {
			return count, nil
		}
	}
}

func (reader *chunkReader) Close() error { return reader.closeChunk() }

// open requests the next chunk and accepts it only when the host served
// exactly the range dac asked for, so bytes can never be stitched together out
// of order or across two versions of an asset.
func (reader *chunkReader) open() error {
	last := min(reader.offset+reader.downloader.chunkSize, reader.total) - 1
	httpRequest, err := reader.downloader.newRequestTo(reader.ctx, reader.request, reader.target, reader.configured)
	if err != nil {
		return err
	}
	httpRequest.Header.Set(rangeHeader, byteRange(reader.offset, last))
	if reader.validator != "" {
		httpRequest.Header.Set(ifRangeHeader, reader.validator)
	}
	response, err := reader.downloader.doRequest(reader.ctx, reader.request.Name, httpRequest)
	if err != nil {
		return err
	}
	if err := reader.accept(response, last); err != nil {
		_ = response.Body.Close()
		return fault.NewNetworkError(err, fault.WithAsset(reader.request.Name))
	}
	return nil
}

// accept validates a chunk response against the range dac requested. A 200
// answer to a conditional range means the asset changed, so the bytes already
// hashed and the bytes now offered belong to different files.
func (reader *chunkReader) accept(response *http.Response, last int64) error {
	if response.StatusCode != http.StatusPartialContent {
		return newRangeError(fmt.Sprintf("requested bytes %d-%d and received HTTP %d", reader.offset, last, response.StatusCode))
	}
	declared := response.Header.Get(contentRangeHeader)
	first, servedLast, complete, err := parseContentRange(declared)
	if err != nil || first != reader.offset || servedLast > last || complete != reader.total {
		return newRangeError(fmt.Sprintf("requested bytes %d-%d/%d and received %q", reader.offset, last, reader.total, declared))
	}
	reader.adopt(response)
	return nil
}

// adopt installs a validated response as the current chunk.
func (reader *chunkReader) adopt(response *http.Response) {
	reader.body = guardIdleReads(reader.ctx, reader.cancel, reader.request.Policy, response.Body)
	reader.served = 0
}

// closeChunk releases the current chunk. Reading a chunk to its end before
// closing it is what lets the transport keep the connection for the next one.
func (reader *chunkReader) closeChunk() error {
	if reader.body == nil {
		return nil
	}
	body := reader.body
	reader.body = nil
	return body.Close()
}

// resume reports whether err is a transport failure dac may continue from the
// offset it has already accepted, and spends one unit of the budget when it
// is. A cancelled context, including dac's own idle timeout, is deliberate and
// never retried, and neither is a failure the host will repeat.
func (reader *chunkReader) resume(err error) bool {
	if reader.resumes == 0 || reader.ctx.Err() != nil {
		return false
	}
	if errors.Is(err, ErrDownloadIdleTimeout) || errors.Is(err, ErrDownloadRangeMismatch) || errors.Is(err, ErrDownloadStatus) {
		return false
	}
	var operation *fault.Error
	if errors.As(err, &operation) && operation.Kind() != fault.ErrorKindNetwork {
		return false
	}
	reader.resumes--
	return true
}

// byteRange renders one closed range in the single-range form of RFC 9110.
func byteRange(first, last int64) string {
	return "bytes=" + strconv.FormatInt(first, 10) + "-" + strconv.FormatInt(last, 10)
}

// parseContentRange reads the "bytes first-last/complete" form of RFC 9110's
// Content-Range. A host that declares its complete length unknown is reported
// as an error, because dac plans every later chunk against that length.
func parseContentRange(value string) (first, last, complete int64, err error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, contentRangeUnit) {
		return 0, 0, 0, errUnparsableContentRange
	}
	span, declared, found := strings.Cut(strings.TrimSpace(trimmed[len(contentRangeUnit):]), "/")
	if !found {
		return 0, 0, 0, errUnparsableContentRange
	}
	firstValue, lastValue, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, errUnparsableContentRange
	}
	if first, err = strconv.ParseInt(strings.TrimSpace(firstValue), 10, 64); err != nil {
		return 0, 0, 0, errUnparsableContentRange
	}
	if last, err = strconv.ParseInt(strings.TrimSpace(lastValue), 10, 64); err != nil {
		return 0, 0, 0, errUnparsableContentRange
	}
	if complete, err = strconv.ParseInt(strings.TrimSpace(declared), 10, 64); err != nil {
		return 0, 0, 0, errUnparsableContentRange
	}
	if first < 0 || last < first || complete <= last {
		return 0, 0, 0, errUnparsableContentRange
	}
	return first, last, complete, nil
}

// rangeValidator returns the strongest identity the host offered for the bytes
// it just served. A weak validator would let a host satisfy a later chunk from
// a different version of the asset, which is exactly what If-Range must catch,
// so only a strong entity tag or a modification date is used.
func rangeValidator(header http.Header) string {
	tag := strings.TrimSpace(header.Get("ETag"))
	if tag != "" && !strings.HasPrefix(tag, "W/") {
		return tag
	}
	return strings.TrimSpace(header.Get("Last-Modified"))
}

// finalURL reports the location that answered a request, which differs from
// the requested one when the transport followed a redirect.
func finalURL(requested *url.URL, response *http.Response) *url.URL {
	if response.Request == nil || response.Request.URL == nil {
		return requested
	}
	return response.Request.URL
}
