// Package application implements DAC operations.
package application

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

const Version = "11.0.0"

// Object identifies bytes in the object store.
type Object struct {
	Digest string
	Size   int64
}

// PutOptions defines the checks for one object installation.
// Build it with PutAny or PutExact rather than by hand.
type PutOptions struct {
	// Expect is what the bytes must turn out to be.
	Expect Object
	// MaxSize bounds an installation whose size is not known ahead of time.
	MaxSize int64
	// Locked reports that the caller already holds the digest lock.
	Locked bool
}

// PutAny installs bytes whose size is not known ahead of time, bounded by maxSize.
func PutAny(digest string, maxSize int64) PutOptions {
	return PutOptions{Expect: Object{Digest: digest, Size: Unknown}, MaxSize: maxSize}
}

// PutExact installs bytes that must match object exactly.
func PutExact(object Object) PutOptions {
	return PutOptions{Expect: object, Locked: true}
}

// ObjectStore is the cache boundary used by the service.
type ObjectStore interface {
	Path(digest string) (string, error)
	// Stat reports the object stored for a digest, confirms that it still holds the bytes DAC installed, and refreshes its liveness timestamp.
	Stat(digest string) (Object, bool, error)
	// Verify hashes one object in full, ignoring anything the store has already decided about it.
	Verify(ctx context.Context, digest string) (Object, bool, error)
	List(ctx context.Context) ([]string, error)
	// Describe reports an object's size and last use without refreshing its liveness timestamp, so asking what the cache holds does not count as using it.
	Describe(digest string) (ObjectDescription, bool, error)
	Remove(ctx context.Context, digest string) error
	Put(ctx context.Context, reader io.Reader, options PutOptions) (Object, error)
	WithLock(ctx context.Context, digest string, operation func() error) error
	GC(ctx context.Context, options GCOptions) (GCResult, error)
}

// cached reports whether the cache holds exactly the expected bytes.
func (service *Service) cached(object Object) (bool, error) {
	stored, found, err := service.Store.Stat(object.Digest)
	if err != nil {
		return false, cacheReadError(err)
	}
	return found && stored.Size == object.Size, nil
}

// usable reports whether the cache holds these bytes, treating a corrupt object as absent.
// Fetching commands treat corruption as a miss so the transfer repairs it.
func (service *Service) usable(object Object) (bool, error) {
	valid, err := service.cached(object)
	if err != nil {
		if corrupted(err) {
			return false, nil
		}
		return false, err
	}
	return valid, nil
}

func (service *Service) objectPath(digest string) (string, error) {
	path, err := service.Store.Path(digest)
	if err != nil {
		return "", cacheReadError(err)
	}
	return path, nil
}

// FetchRequest describes one remote asset request.
type FetchRequest struct {
	URL               string
	ETag              string
	AllowInsecureHTTP bool
}

// FetchResponse contains the response fields used by DAC.
type FetchResponse struct {
	NotModified bool
	ETag        string
	// Filename is the name the origin gives this asset, already reduced to a safe single path element, or empty when the origin names none.
	Filename string
	Length   int64
	Body     io.ReadCloser
}

// Fetcher is the remote source boundary used by the service.
type Fetcher interface {
	Fetch(ctx context.Context, request FetchRequest) (*FetchResponse, error)
}

// HostTrust reports whether DAC is willing to download from a host.
// Info answers without making a request, so it asks the trusted-hosts file
// rather than the transport stage that enforces the same answer.
type HostTrust interface {
	Trusted(host string) bool
}

// Reporter is the progress boundary used by the service.
type Reporter interface {
	// Plan names the assets the operation about to run will report on, before it reports on any of them.
	// It is a set to expect rather than a promise: an operation may settle an asset without a transfer to report, and calling this at all is optional.
	Plan(names []string)
	Start(name string, total int64)
	Advance(name string, count int64)
	Done(name, status string)
	Fail(name string, err error)
	// Wait ends the display and returns once it has finished with it.
	// A started asset is not promised a Done or a Fail: the first failure of an operation cancels the transfers running beside it, and those are reported as nothing at all, because the only message they could carry would name another asset's failure rather than anything about themselves.
	// So this is where a reporter holding state per asset disposes of whatever the operation did not settle, and one that waits for those to settle instead waits for good.
	// It is called once, after the operation's transfers have finished.
	Wait()
}

// Service runs commands against one project.
type Service struct {
	ManifestPath string
	LockPath     string
	Store        ObjectStore
	Fetcher      Fetcher
	Reporter     Reporter
	// Trust reports which hosts a download would be allowed to reach. A nil value
	// reports nothing rather than reporting that nothing is trusted.
	Trust HostTrust
}

// New creates a service with absolute project paths.
func New(manifestPath, lockPath string, store ObjectStore, fetcher Fetcher, reporter Reporter) *Service {
	manifestPath, _ = filepath.Abs(manifestPath)
	lockPath, _ = filepath.Abs(lockPath)
	if reporter == nil {
		reporter = NopReporter{}
	}
	return &Service{ManifestPath: manifestPath, LockPath: lockPath, Store: store, Fetcher: fetcher, Reporter: reporter}
}

func (service *Service) readManifest() (project.Manifest, error) {
	manifest, err := project.ReadManifest(service.ManifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return project.Manifest{}, fault.New("manifest_missing", "The manifest file does not exist. Run dac init.")
	}
	if err != nil {
		return project.Manifest{}, fault.Wrap("manifest_invalid", "The manifest file is invalid.", err)
	}
	return manifest, nil
}

func (service *Service) readProject() (project.Manifest, project.Lock, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return project.Manifest{}, project.Lock{}, err
	}
	lock, err := project.ReadLock(service.LockPath)
	if errors.Is(err, os.ErrNotExist) {
		return project.Manifest{}, project.Lock{}, fault.New("lock_missing", "The lock file does not exist. Run dac pull.")
	}
	if err != nil {
		return project.Manifest{}, project.Lock{}, fault.Wrap("lock_invalid", "The lock file is invalid.", err)
	}
	if err := project.CheckLock(manifest, lock); err != nil {
		return project.Manifest{}, project.Lock{}, fault.Wrap("lock_stale", "The lock file does not agree with the manifest. Run dac pull --refresh.", err)
	}
	return manifest, lock, nil
}

// transferReader is the asset body as the object store reads it.
// It reports progress, and it marks a read failure as a transfer failure, because
// one Put call both reads the network and writes the cache: by the time the error
// comes back out, this reader is the only thing that knew which of the two it was.
type transferReader struct {
	name     string
	reader   io.Reader
	reporter Reporter
}

func (reader *transferReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		reader.reporter.Advance(reader.name, int64(count))
	}
	// The end of the body is passed back exactly as it arrived: io.Copy compares it against io.EOF rather than unwrapping it, so a wrapped one would read as a failure.
	if err != nil && !errors.Is(err, io.EOF) {
		return count, &transferError{Err: err}
	}
	return count, err
}

// NopReporter discards progress.
type NopReporter struct{}

func (NopReporter) Plan([]string)         {}
func (NopReporter) Start(string, int64)   {}
func (NopReporter) Advance(string, int64) {}
func (NopReporter) Done(string, string)   {}
func (NopReporter) Fail(string, error)    {}
func (NopReporter) Wait()                 {}
