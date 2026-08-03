package application

import (
	"context"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

func (service *Service) resolve(ctx context.Context, coordinate coord.Coordinate, source project.Asset, old project.LockAsset, options NetworkOptions) (project.LockAsset, string, error) {
	name := coordinate.String()
	oldMatches := old.URL == source.URL && (source.Integrity == "" || old.Digest == source.Integrity)
	// Only an asset the manifest leaves unpinned is ever revalidated. A
	// publisher digest already settles which bytes are correct, so a pinned
	// asset is answered from the cache or downloaded outright, and it neither
	// sends nor records an ETag.
	conditional := source.Integrity == ""
	// A publisher digest that the cache already satisfies lets lock finish
	// without a request, which also means it never proves the URL still serves
	// those bytes. Refresh trades that speed for a check against the origin.
	if source.Integrity != "" && !options.Refresh {
		object, found, err := service.Store.Stat(source.Integrity)
		// A corrupt object cannot answer the request, but lock can: it falls
		// through to the origin and installs good bytes over the bad ones.
		if err != nil && !corrupted(err) {
			return project.LockAsset{}, "", cacheReadError(err)
		}
		if err == nil && found {
			service.Reporter.Start(name, object.Size)
			service.Reporter.Done(name, "cached")
			return project.LockAsset{URL: source.URL, Digest: object.Digest, Size: object.Size}, "cached", nil
		}
	}
	// Refresh does not suppress the hint. An origin that answers 304 to the
	// stored ETag has confirmed the asset just as well as one that sends the
	// bytes again, and for a large asset that is the difference between a check
	// that runs on a schedule and one nobody runs at all. What refresh does
	// suppress is the shortcut above, which answers without asking the origin
	// anything at all.
	oldValid := false
	if oldMatches {
		var err error
		oldValid, err = service.usable(Object{Digest: old.Digest, Size: old.Size})
		if err != nil {
			return project.LockAsset{}, "", err
		}
	}
	hint := ""
	if conditional && oldValid {
		hint = old.ETag
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
		return old, "not_modified", nil
	}
	service.Reporter.Start(name, response.Length)
	reader := &progressReader{name: name, reader: response.Body, reporter: service.Reporter}
	// A pinned asset is normally installed against its publisher digest, so
	// bytes that fail it never reach the cache. A caller that is observing wants
	// the digest the origin actually served, because the difference between that
	// and the locked one is its whole answer, and the store cannot report it
	// while it is also refusing it.
	expect := source.Integrity
	if options.Observe {
		expect = ""
	}
	object, err := service.Store.Put(ctx, reader, PutAny(expect, options.MaxSize))
	if err != nil {
		return project.LockAsset{}, "", contentError(err)
	}
	service.Reporter.Done(name, "resolved")
	etag := ""
	if conditional {
		etag = response.ETag
	}
	return project.LockAsset{
		URL:    source.URL,
		Digest: object.Digest,
		Size:   object.Size,
		ETag:   etag,
	}, "resolved", nil
}

func (service *Service) fetch(ctx context.Context, source project.Asset, etag string) (*FetchResponse, error) {
	if service.Fetcher == nil {
		return nil, fault.New("network_error", "No asset fetcher is configured.")
	}
	response, err := service.Fetcher.Fetch(ctx, FetchRequest{URL: source.URL, ETag: etag, AllowInsecureHTTP: source.AllowInsecureHTTP})
	if err != nil {
		return nil, networkError(err)
	}
	return response, nil
}
