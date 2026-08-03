package application

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// PullResult reports a pull operation.
type PullResult struct {
	ManifestDigest string `json:"manifestDigest"`
	AssetSummary
}

// Pull installs every missing locked asset.
func (service *Service) Pull(ctx context.Context, options NetworkOptions) (PullResult, error) {
	defer service.Reporter.Wait()
	manifest, lock, err := service.readProject()
	if err != nil {
		return PullResult{}, err
	}
	names := manifest.Names()
	assets, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name string) (Asset, error) {
		value, err := service.pull(ctx, name, manifest.Assets[name], lock.Assets[name], options)
		if err != nil && ctx.Err() == nil {
			service.Reporter.Fail(name, err)
		}
		return value, err
	})
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{ManifestDigest: lock.ManifestDigest, AssetSummary: collect(assets)}, nil
}

func (service *Service) pull(ctx context.Context, name string, source project.Asset, locked project.LockAsset, options NetworkOptions) (Asset, error) {
	object := Object{Digest: locked.Digest, Size: locked.Size}
	// The view already answers whether the cache holds these bytes, so a hit
	// costs one lookup rather than one to decide and another to report.
	view, err := service.assetView(name, source, locked, "cached")
	if err != nil {
		return Asset{}, err
	}
	service.Reporter.Start(name, locked.Size)
	if view.Cached {
		service.Reporter.Done(name, "cached")
		return view, nil
	}
	status := "downloaded"
	err = service.Store.WithLock(ctx, locked.Digest, func() error {
		valid, err := service.cached(object)
		if err != nil {
			return err
		}
		if valid {
			status = "cached"
			service.Reporter.Done(name, status)
			return nil
		}
		if options.DistDir != "" {
			found, err := service.installDist(ctx, options.DistDir, source, locked)
			if err != nil {
				return err
			}
			if found {
				status = "distdir"
				service.Reporter.Done(name, status)
				return nil
			}
		}
		if options.Offline {
			return fault.New("offline_cache_miss", "Offline mode could not find the required cache object.")
		}
		response, err := service.fetch(ctx, project.Asset{URL: source.URL, AllowInsecureHTTP: source.AllowInsecureHTTP}, "")
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.NotModified {
			return fault.New("network_error", "The server returned an invalid not-modified response.")
		}
		if response.Length >= 0 && response.Length != locked.Size {
			mismatch := &ContentError{ExpectedDigest: locked.Digest, ExpectedSize: locked.Size, ActualSize: response.Length}
			return &fault.Error{
				Code:    "content_size_mismatch",
				Message: "The downloaded asset size does not match the lock file: " + mismatch.Error() + ".",
				Details: mismatch.Details(),
			}
		}
		reader := &progressReader{name: name, reader: response.Body, reporter: service.Reporter}
		if _, err := service.Store.Put(ctx, reader, PutExact(object)); err != nil {
			return contentError(err)
		}
		service.Reporter.Done(name, status)
		return nil
	})
	if err != nil {
		return Asset{}, err
	}
	return service.assetView(name, source, locked, status)
}

// installDist installs one locked asset from a distribution directory. The
// caller holds the digest lock.
//
// A file named for the digest is a claim that DAC checks and refuses when it is
// wrong, because a bundle that names the wrong bytes is a broken bundle. A file
// named after the URL is only a guess, so a mismatch there falls through to the
// next candidate rather than failing the command.
func (service *Service) installDist(ctx context.Context, directory string, source project.Asset, locked project.LockAsset) (bool, error) {
	hexValue, err := digest.Hex(locked.Digest)
	if err != nil {
		return false, fault.Wrap("lock_invalid", "The lock file has an invalid digest.", err)
	}
	candidates := []struct {
		path   string
		strict bool
	}{{path: filepath.Join(directory, hexValue), strict: true}}
	if base := urlBase(source.URL); base != "" {
		candidates = append(candidates, struct {
			path   string
			strict bool
		}{path: filepath.Join(directory, base)})
	}
	for _, candidate := range candidates {
		file, err := os.Open(candidate.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fault.Wrap("distdir_read_failed", "DAC could not read the distribution directory.", err)
		}
		_, err = service.Store.Put(ctx, file, PutExact(Object{Digest: locked.Digest, Size: locked.Size}))
		_ = file.Close()
		if err == nil {
			return true, nil
		}
		var mismatch *ContentError
		if !candidate.strict && errors.As(err, &mismatch) {
			continue
		}
		return false, contentError(err)
	}
	return false, nil
}

// urlBase returns the final path element of a URL, or an empty string when the
// URL has none worth using as a file name.
func urlBase(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	base := path.Base(parsed.Path)
	if base == "." || base == "/" || base == "" || strings.ContainsRune(base, filepath.Separator) {
		return ""
	}
	return base
}
