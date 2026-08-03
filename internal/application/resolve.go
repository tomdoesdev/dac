package application

import (
	"context"

	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

func (service *Service) resolve(ctx context.Context, name string, source project.Asset, old project.LockAsset, options NetworkOptions) (project.LockAsset, string, error) {
	oldMatches := old.URL == source.URL && (source.Integrity == "" || old.Digest == source.Integrity)
	old.Version = source.Version
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
		if err != nil {
			return project.LockAsset{}, "", cacheReadError(err)
		}
		if found {
			service.Reporter.Start(name, object.Size)
			service.Reporter.Done(name, "cached")
			return project.LockAsset{Version: source.Version, URL: source.URL, Digest: object.Digest, Size: object.Size}, "cached", nil
		}
	}
	oldValid := false
	if oldMatches && !options.Refresh {
		var err error
		oldValid, err = service.cached(Object{Digest: old.Digest, Size: old.Size})
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
	object, err := service.Store.Put(ctx, reader, PutAny(source.Integrity, options.MaxSize))
	if err != nil {
		return project.LockAsset{}, "", contentError(err)
	}
	service.Reporter.Done(name, "resolved")
	etag := ""
	if conditional {
		etag = response.ETag
	}
	return project.LockAsset{
		Version: source.Version,
		URL:     source.URL,
		Digest:  object.Digest,
		Size:    object.Size,
		ETag:    etag,
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
