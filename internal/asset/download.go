package asset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"github.com/tomdoesdev/dac/internal/project"
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
	headers, err := requestHeaders(request.Headers)
	if err != nil {
		return nil, &project.Error{Kind: "configuration", Asset: request.Name, Err: err}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return nil, &project.Error{Kind: "configuration", Asset: request.Name, Err: errors.New("cannot create HTTP request")}
	}
	httpRequest.Header = headers
	httpRequest.Header.Set("User-Agent", downloader.userAgent)
	response, err := downloader.client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, &project.Error{Kind: "cancelled", Asset: request.Name, Err: context.Canceled}
		}
		return nil, &project.Error{Kind: "network", Asset: request.Name, Err: errors.New("failed to download from remote host")}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &project.Error{Kind: "network", Asset: request.Name, Err: fmt.Errorf("server returned HTTP %d", response.StatusCode)}
	}
	temporary, err := atomic.CreateRoot(downloads, request.File, 0o644)
	if err != nil {
		return nil, project.NewError("filesystem", err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temporary, hash), response.Body)
	if copyErr != nil {
		cleanupErr := temporary.Discard()
		return nil, errors.Join(&project.Error{Kind: "network", Asset: request.Name, Err: errors.New("failed while downloading response body")}, project.NewError("filesystem", cleanupErr))
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if expected != "" {
		normalized, _ := NormalizeDigest(expected)
		if digest != normalized {
			cleanupErr := temporary.Discard()
			return nil, errors.Join(&project.Error{Kind: "integrity", Asset: request.Name, Expected: normalized, Received: digest, Err: errors.New("downloaded bytes do not match expected digest")}, project.NewError("filesystem", cleanupErr))
		}
	}
	return &StagedDownload{file: temporary, Digest: digest, Size: size}, nil
}

// VerifyLocal hashes a regular managed file only when its size can match. A
// symlink is intentionally untrusted: later replacement must not follow it.
func VerifyLocal(downloads *os.Root, name, expected string, size int64) (bool, error) {
	info, err := downloads.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, project.NewError("filesystem", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return false, nil
	}
	file, err := downloads.Open(name)
	if err != nil {
		return false, project.NewError("filesystem", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, project.NewError("filesystem", err)
	}
	return "sha256:"+hex.EncodeToString(hash.Sum(nil)) == expected, nil
}
