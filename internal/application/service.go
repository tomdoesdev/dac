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

const Version = "9.0.0"

// Object identifies bytes in the object store.
type Object struct {
	Digest string
	Size   int64
}

// PutOptions defines the checks for one object installation.
//
// Build it with PutAny or PutExact rather than by hand. The zero value asks for
// an empty object, because zero is a legitimate size, so a struct literal that
// forgets Size fails every install instead of accepting any size.
type PutOptions struct {
	// Expect is what the bytes must turn out to be. An empty Digest accepts any
	// digest, and a Size of Unknown accepts any size.
	Expect Object
	// MaxSize bounds an installation whose size is not known ahead of time.
	MaxSize int64
	// Locked reports that the caller already holds the digest lock.
	Locked bool
}

// PutAny installs bytes whose size is not known ahead of time, bounded by
// maxSize. An empty digest accepts whatever arrives, which is how lock resolves
// an asset that carries no publisher integrity value.
func PutAny(digest string, maxSize int64) PutOptions {
	return PutOptions{Expect: Object{Digest: digest, Size: Unknown}, MaxSize: maxSize}
}

// PutExact installs bytes that must match object exactly. It assumes the digest
// lock, which every caller that already knows the digest takes before it
// decides whether it needs to download anything at all.
func PutExact(object Object) PutOptions {
	return PutOptions{Expect: object, Locked: true}
}

// ObjectStore is the cache boundary used by the service.
type ObjectStore interface {
	Path(digest string) (string, error)
	// Stat reports the object stored for a digest, confirms that it still holds
	// the bytes DAC installed, and refreshes its liveness timestamp. It reports
	// a CorruptError for an object that fails that check.
	Stat(digest string) (Object, bool, error)
	// Verify hashes one object in full, ignoring anything the store has already
	// decided about it.
	Verify(ctx context.Context, digest string) (Object, bool, error)
	List(ctx context.Context) ([]string, error)
	// Describe reports an object's size and last use without refreshing its
	// liveness timestamp, so asking what the cache holds does not count as
	// using it.
	Describe(digest string) (ObjectDescription, bool, error)
	Remove(ctx context.Context, digest string) error
	Put(ctx context.Context, reader io.Reader, options PutOptions) (Object, error)
	WithLock(ctx context.Context, digest string, operation func() error) error
	GC(ctx context.Context, options GCOptions) (GCResult, error)
}

// cached reports whether the cache holds exactly the expected bytes. A stored
// object of another size is a different object, not this one. An object that
// fails its integrity check is an error, not a miss.
func (service *Service) cached(object Object) (bool, error) {
	stored, found, err := service.Store.Stat(object.Digest)
	if err != nil {
		return false, cacheReadError(err)
	}
	return found && stored.Size == object.Size, nil
}

// usable reports whether the cache holds these bytes, treating a corrupt object
// as absent.
//
// It belongs to the commands that can fetch the object again: replacing bad
// bytes repairs the cache, which is a better answer than refusing to run over
// damage the command was about to undo anyway. Commands with no such recourse
// call cached and report the failure.
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
	// Filename is the name the origin gives this asset, already reduced to a
	// safe single path element, or empty when the origin names none. The
	// adapter derives it because the spelling is an HTTP concern; what to
	// record is not, and that decision stays in resolve.
	Filename string
	Length   int64
	Body     io.ReadCloser
}

// Fetcher is the remote source boundary used by the service.
type Fetcher interface {
	Fetch(ctx context.Context, request FetchRequest) (*FetchResponse, error)
}

// FetcherDecorator adds source behavior before the terminal fetcher runs.
// The first decorator is the outermost wrapper.
type FetcherDecorator func(Fetcher) Fetcher

// DecorateFetcher builds the source stage of the download pipeline.
func DecorateFetcher(fetcher Fetcher, decorators ...FetcherDecorator) Fetcher {
	for index := len(decorators) - 1; index >= 0; index-- {
		fetcher = decorators[index](fetcher)
	}
	return fetcher
}

// Reporter is the progress boundary used by the service.
type Reporter interface {
	// Plan names the assets the operation about to run will report on, before
	// it reports on any of them. A reporter that lays its output out in columns
	// cannot size the one holding these names from the names it has been given
	// so far without moving what it has already drawn every time a longer one
	// arrives, and the operation knows the whole set before it starts.
	//
	// It is a set to expect rather than a promise: an operation may settle an
	// asset without a transfer to report, and calling this at all is optional.
	Plan(names []string)
	Start(name string, total int64)
	Advance(name string, count int64)
	Done(name, status string)
	Fail(name string, err error)
	Wait()
}

// Service runs commands against one project.
type Service struct {
	ManifestPath string
	LockPath     string
	Store        ObjectStore
	Fetcher      Fetcher
	Reporter     Reporter
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
		return project.Manifest{}, project.Lock{}, fault.New("lock_missing", "The lock file does not exist. Run dac lock.")
	}
	if err != nil {
		return project.Manifest{}, project.Lock{}, lockFault("lock_invalid", "The lock file is invalid.", err)
	}
	if err := project.CheckLock(manifest, lock); err != nil {
		return project.Manifest{}, project.Lock{}, fault.Wrap("lock_stale", "The lock file does not agree with the manifest. Run dac lock.", err)
	}
	return manifest, lock, nil
}

type progressReader struct {
	name     string
	reader   io.Reader
	reporter Reporter
}

func (reader *progressReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		reader.reporter.Advance(reader.name, int64(count))
	}
	return count, err
}

// NopReporter discards progress. It backs both a service built without a
// reporter and the --progress=false form of every network command.
type NopReporter struct{}

func (NopReporter) Plan([]string)         {}
func (NopReporter) Start(string, int64)   {}
func (NopReporter) Advance(string, int64) {}
func (NopReporter) Done(string, string)   {}
func (NopReporter) Fail(string, error)    {}
func (NopReporter) Wait()                 {}
