package application

import (
	"context"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/util/filename"
)

func (service *Service) resolve(ctx context.Context, coordinate coord.Coordinate, source project.Asset, old project.LockAsset, options reconcileOptions) (project.LockAsset, string, error) {
	name := coordinate.String()
	oldMatches := old.URL == source.URL && (source.Integrity == "" || old.Digest == source.Integrity)
	// Only an asset the manifest leaves unpinned is ever revalidated.
	conditional := source.Integrity == ""
	// A publisher digest that the cache already satisfies lets lock finish without a request, which also means it never proves the URL still serves those bytes.
	if source.Integrity != "" && options.mode == resolveChanged {
		object, found, err := service.Store.Stat(source.Integrity)
		// A corrupt object cannot answer the request, but lock can: it falls through to the origin and installs good bytes over the bad ones.
		if err != nil && !corrupted(err) {
			return project.LockAsset{}, "", cacheReadError(err)
		}
		if err == nil && found {
			service.Reporter.Start(name, object.Size)
			service.Reporter.Done(name, "cached")
			return project.LockAsset{
				URL:      source.URL,
				Digest:   object.Digest,
				Size:     object.Size,
				Filename: resolvedFilename("", source, old),
			}, "cached", nil
		}
	}
	// Refresh does not suppress the hint.
	oldValid := false
	if oldMatches {
		var err error
		oldValid, err = service.usable(Object{Digest: old.Digest, Size: old.Size})
		if err != nil {
			return project.LockAsset{}, "", err
		}
	}
	hint := project.LockAsset{}
	if conditional && oldValid {
		hint = old
	}
	if options.offline {
		return project.LockAsset{}, "", fault.New("offline_cache_miss",
			"Offline mode cannot resolve a changed asset that has no reusable digest.")
	}
	response, err := service.fetch(ctx, source, hint)
	if err != nil {
		return project.LockAsset{}, "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.NotModified {
		service.Reporter.Start(name, old.Size)
		service.Reporter.Done(name, "not_modified")
		if response.ETag != "" {
			old.ETag = response.ETag
		}
		if response.LastModified != "" {
			old.LastModified = response.LastModified
		}
		// A name the manifest declares is applied here too, because it is the project's own answer and a 304 says nothing about it either way.
		if declared := filename.Clean(source.Filename); declared != "" {
			old.Filename = declared
		} else if old.Filename == "" {
			old.Filename = resolvedFilename(response.Filename, source, old)
		}
		return old, "not_modified", nil
	}
	service.Reporter.Start(name, response.Length)
	reader := &transferReader{name: name, reader: response.Body, reporter: service.Reporter}
	// A publisher pin remains the acceptance rule even during a pull refresh, so
	// bytes that violate it never enter the cache or lock.
	object, err := service.Store.Put(ctx, reader, PutAny(source.Integrity, options.maxSize))
	if err != nil {
		return project.LockAsset{}, "", contentError(err)
	}
	service.Reporter.Done(name, "resolved")
	value := project.LockAsset{
		URL:          source.URL,
		Digest:       object.Digest,
		Size:         object.Size,
		ETag:         response.ETag,
		LastModified: response.LastModified,
		Filename:     resolvedFilename(response.Filename, source, old),
	}
	// These bytes are in the cache now, so this is where they are recorded, rather than only
	// once the operation around them settles. An operation that is cancelled, or that gives up
	// because a different asset failed, still leaves behind everything it had already installed.
	service.noteAsset(coordinate, value)
	return value, "resolved", nil
}

// resolvedFilename returns the best name known for an asset.
// A manifest name wins because it is an explicit project decision.
// A name is only carried over from the old entry while the URL is unchanged.
// Both the declared and the supplied name are cleaned again here rather than assumed safe.
func resolvedFilename(supplied string, source project.Asset, old project.LockAsset) string {
	if name := filename.Clean(source.Filename); name != "" {
		return name
	}
	if name := filename.Clean(supplied); name != "" {
		return name
	}
	if old.URL == source.URL && old.Filename != "" {
		return old.Filename
	}
	return filename.FromURL(source.URL)
}

func (service *Service) fetch(ctx context.Context, source project.Asset, validator project.LockAsset) (*FetchResponse, error) {
	if service.Fetcher == nil {
		return nil, fault.New("network_error", "No asset fetcher is configured.")
	}
	response, err := service.Fetcher.Fetch(ctx, FetchRequest{
		URL: source.URL, ETag: validator.ETag, LastModified: validator.LastModified,
	})
	if err != nil {
		return nil, networkError(err)
	}
	return response, nil
}
