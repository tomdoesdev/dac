// Command dacserve serves synthetic assets over HTTP so that DAC can be
// exercised against a local origin.
//
// An asset is a size, a seed, and an optional rate limit. Its bytes are
// generated on demand and never stored, and they are a pure function of the
// asset name and the seed, so the same configuration always serves the same
// bytes: a lock file written against this server stays valid across a restart,
// and changing a seed is how you make an asset drift the way a publisher who
// rewrites bytes at a stable URL does.
//
// This is a test utility. It is not part of DAC, it trusts its configuration
// file, and it should only ever listen on loopback.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tomdoesdev/kit/bytesize"
)

// defaultAddr is loopback, which is where DAC accepts plain HTTP without a
// flag, and the only place a server that invents its own content belongs.
const defaultAddr = "127.0.0.1:8080"

const (
	// syntheticModifiedEpoch is 2000-01-01T00:00:00Z. Keeping generated
	// modification times in a fixed historical window makes them useful for
	// conditional requests without ever presenting a future timestamp.
	syntheticModifiedEpoch int64 = 946684800
	// Twenty ordinary years keep every generated timestamp before 2020 while
	// providing second-level variation between different synthetic assets.
	syntheticModifiedWindow uint64 = 20 * 365 * 24 * 60 * 60
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	configPath := flag.String("config", "dacserve.json", "Read this JSON configuration file.")
	addr := flag.String("addr", "", "Listen on this address instead of the configured one.")
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flag.Arg(0))
	}

	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		config.Addr = *addr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, config, logger)
}

// serve listens and serves until the context is cancelled.
func serve(ctx context.Context, config serverConfig, logger *log.Logger) error {
	mux, origins := newHandler(config, logger)
	listener, err := net.Listen("tcp", config.Addr)
	if err != nil {
		return err
	}
	for _, origin := range origins {
		logger.Printf("serving http://%s/%s  %d bytes  %s", listener.Addr(), origin.name, origin.size, rateText(origin.rate))
	}

	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		// Close rather than Shutdown: a throttled download is meant to be slow,
		// so waiting for one to finish would turn Ctrl-C into a hang.
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newHandler builds the request multiplexer and the assets it serves.
func newHandler(config serverConfig, logger *log.Logger) (http.Handler, []*origin) {
	mux := http.NewServeMux()
	origins := make([]*origin, 0, len(config.Assets))
	for _, name := range slices.Sorted(maps.Keys(config.Assets)) {
		origin := newOrigin(name, config.Assets[name], config.Rate, logger)
		// A GET pattern answers HEAD too, which is how a client asks for the
		// size without paying for the bytes.
		mux.HandleFunc("GET /"+name, origin.serve)
		origins = append(origins, origin)
	}
	return mux, origins
}

// serverConfig is the dacserve configuration file.
type serverConfig struct {
	// Addr is the listen address, such as 127.0.0.1:8080. Port 0 picks a free
	// port, which the startup log then reports.
	Addr string `json:"addr"`
	// Rate is the default limit in bytes per second. Zero is no limit.
	Rate byteCount `json:"rate"`
	// Assets maps a URL path to the asset served there.
	Assets map[string]assetConfig `json:"assets"`
}

// assetConfig defines one synthetic asset.
type assetConfig struct {
	// Size is the exact length of the asset in bytes.
	Size byteCount `json:"size"`
	// Rate limits this asset in bytes per second. Zero takes the file's rate.
	Rate byteCount `json:"rate"`
	// Seed selects which bytes the asset has. Changing it changes the content
	// and the ETag, and nothing else.
	Seed int64 `json:"seed"`
}

func readConfig(path string) (serverConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return serverConfig{}, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	// An unknown field is a typo in a file nothing else validates, and serving
	// something other than what the file says is worse than refusing to start.
	decoder.DisallowUnknownFields()
	var parsed serverConfig
	if err := decoder.Decode(&parsed); err != nil {
		return serverConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	parsed.applyDefaults()
	if err := parsed.validate(); err != nil {
		return serverConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parsed, nil
}

// applyDefaults fills in what the configuration file left out.
func (config *serverConfig) applyDefaults() {
	if config.Addr == "" {
		config.Addr = defaultAddr
	}
}

// validate rejects a configuration dacserve could not serve.
func (config serverConfig) validate() error {
	if config.Rate < 0 {
		return errors.New("rate must not be negative")
	}
	if len(config.Assets) == 0 {
		return errors.New("no assets are defined")
	}
	for name, asset := range config.Assets {
		if err := checkName(name); err != nil {
			return err
		}
		if asset.Size < 0 {
			return fmt.Errorf("asset %q has a negative size", name)
		}
		if asset.Rate < 0 {
			return fmt.Errorf("asset %q has a negative rate", name)
		}
	}
	return nil
}

// checkName rejects an asset name that would not survive being pasted into a
// routing pattern and a URL.
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New("asset name must not be empty")
	case strings.HasPrefix(name, "/"), strings.HasSuffix(name, "/"):
		return fmt.Errorf("asset name %q must not begin or end with a slash", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("asset name %q must not contain %q", name, "..")
	case strings.ContainsAny(name, " \t{}?#%"):
		return fmt.Errorf("asset name %q must not contain a space or any of {}?#%%", name)
	}
	return nil
}

// origin serves one synthetic asset.
type origin struct {
	name         string
	size         int64
	rate         int64
	etag         string
	lastModified time.Time
	key          [32]byte
	logger       *log.Logger
}

// newOrigin builds one asset. The configuration it comes from is already
// validated, so the name needs no second check here.
func newOrigin(name string, config assetConfig, defaultRate byteCount, logger *log.Logger) *origin {
	rate := config.Rate
	if rate == 0 {
		rate = defaultRate
	}
	created := &origin{
		name:   name,
		size:   int64(config.Size),
		rate:   int64(rate),
		logger: logger,
	}
	created.key = sha256.Sum256(fmt.Appendf(nil, "dacserve/content\x00%s\x00%d", name, config.Seed))
	// The ETag covers exactly the inputs the bytes come from, so it is strong,
	// stable across restarts, and changes whenever the content does.
	sum := sha256.Sum256(fmt.Appendf(nil, "dacserve/etag\x00%s\x00%d\x00%d", name, config.Seed, created.size))
	created.etag = `"` + hex.EncodeToString(sum[:16]) + `"`
	// The remaining digest bytes place Last-Modified at a stable whole second
	// between 2000 and 2020. It therefore follows the same content identity as
	// the ETag without depending on wall-clock time or configuration metadata.
	offset := binary.BigEndian.Uint64(sum[16:24]) % syntheticModifiedWindow
	created.lastModified = time.Unix(syntheticModifiedEpoch+int64(offset), 0).UTC()
	return created
}

func (origin *origin) serve(writer http.ResponseWriter, request *http.Request) {
	header := writer.Header()
	// Set the type so ServeContent has nothing to sniff: random bytes
	// occasionally look like something.
	header.Set("Content-Type", "application/octet-stream")
	header.Set("ETag", origin.etag)

	started := time.Now()
	paced := &pacedWriter{ResponseWriter: writer, ctx: request.Context(), rate: origin.rate, status: http.StatusOK, started: started}
	// ServeContent handles HEAD, ETag and modification-time conditionals,
	// ranges, and Content-Length, so this server only has to produce bytes at
	// an offset.
	http.ServeContent(paced, request, "", origin.lastModified, &blob{key: origin.key, size: origin.size})
	origin.logger.Printf("%s /%s %d %d bytes in %s", request.Method, origin.name, paced.status, paced.written, time.Since(started).Round(time.Millisecond))
}

// blob is a deterministic, seekable stream of pseudorandom bytes.
//
// Each 32-byte block is the digest of the key and the block number, so any
// offset costs one hash and no state. That is what makes the stream seekable,
// which is what lets net/http answer a range request, and what makes two runs
// of the same configuration byte-for-byte identical.
type blob struct {
	key    [32]byte
	size   int64
	offset int64
}

const blockSize = sha256.Size

func (blob *blob) Read(buffer []byte) (int, error) {
	if blob.offset >= blob.size {
		return 0, io.EOF
	}
	if remaining := blob.size - blob.offset; int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	// The counter follows the key in one scratch array so that hashing a block
	// allocates nothing.
	var scratch [blockSize + 8]byte
	copy(scratch[:], blob.key[:])
	read := 0
	for read < len(buffer) {
		binary.BigEndian.PutUint64(scratch[blockSize:], uint64(blob.offset/blockSize))
		block := sha256.Sum256(scratch[:])
		copied := copy(buffer[read:], block[blob.offset%blockSize:])
		read += copied
		blob.offset += int64(copied)
	}
	return read, nil
}

func (blob *blob) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += blob.offset
	case io.SeekEnd:
		offset += blob.size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	blob.offset = offset
	return offset, nil
}

// pacedWriter limits how fast a response body is written.
//
// The schedule is absolute rather than a sleep per chunk, so a slow write or a
// late wakeup is corrected by the next one instead of accumulating.
type pacedWriter struct {
	http.ResponseWriter
	ctx     context.Context
	rate    int64 // Bytes per second. Zero is no limit.
	started time.Time
	written int64
	status  int
}

// Unwrap lets http.NewResponseController reach the real writer to flush it.
func (writer *pacedWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (writer *pacedWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *pacedWriter) Write(buffer []byte) (int, error) {
	if writer.rate <= 0 {
		written, err := writer.ResponseWriter.Write(buffer)
		writer.written += int64(written)
		return written, err
	}
	// A tenth of a second of bytes keeps a slow asset trickling rather than
	// arriving in one lump whenever the caller's buffer happens to fill.
	chunkSize := max(int(writer.rate/10), 512)
	written := 0
	for written < len(buffer) {
		end := min(written+chunkSize, len(buffer))
		count, err := writer.ResponseWriter.Write(buffer[written:end])
		written += count
		writer.written += int64(count)
		if err != nil {
			return written, err
		}
		if err := writer.pace(); err != nil {
			return written, err
		}
	}
	return written, nil
}

// pace waits until the bytes written so far are due.
func (writer *pacedWriter) pace() error {
	due := time.Duration(float64(writer.written) / float64(writer.rate) * float64(time.Second))
	wait := due - time.Since(writer.started)
	if wait < time.Millisecond {
		return nil
	}
	// Flush before sleeping, or the throttle would only be visible to a client
	// that waits for the whole body.
	_ = http.NewResponseController(writer).Flush()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-writer.ctx.Done():
		return writer.ctx.Err()
	case <-timer.C:
		return nil
	}
}

// byteCount is a size in bytes that reads either a JSON number or a string
// with a unit, so a configuration can say "256MiB" instead of 268435456.
//
// A rate reads the same way and may carry a trailing "/s", so that a rate looks
// like a rate. The units are DAC's own, from one parser, so a size that works
// on the command line works here too.
type byteCount int64

func (count *byteCount) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		parsed, err := parseByteRate(text)
		if err != nil {
			return err
		}
		*count = byteCount(parsed)
		return nil
	}
	var number int64
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*count = byteCount(number)
	return nil
}

// parseByteRate accepts DAC's optional per-second marker while keeping rate
// syntax out of the shared byte-count package until another caller needs it.
func parseByteRate(value string) (int64, error) {
	trimmed, _ := strings.CutSuffix(strings.TrimSpace(value), "/s")
	return bytesize.Parse(trimmed)
}

func rateText(rate int64) string {
	if rate <= 0 {
		return "unthrottled"
	}
	return strconv.FormatInt(rate, 10) + " B/s"
}
