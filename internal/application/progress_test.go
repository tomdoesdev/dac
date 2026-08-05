package application_test

// This file is about the seam between an operation and the reporter following
// it, which is where a display and a cancellation rule that are each defensible
// on their own produced a command that never returned.
//
// An operation reports nothing for an asset another asset's failure cut short,
// so that one unreachable origin surfaces as one message rather than as a
// column of cancellations. The terminal reporter draws a bar per asset and
// finishes when every bar it started has ended. Put together, the first failure
// of a concurrent run left bars nobody would ever end, and the deferred wait
// held the process open on a display that had stopped moving. The tests here
// run a real reporter under real operations, because neither package can see
// that on its own.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/progress"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/dac/internal/style"
)

// An unreachable origin is the ordinary way to reach this: a URL whose host
// does not resolve fails as soon as it is asked, while the asset beside it is
// mid-transfer and is cancelled without a word.
//
// Pull starts a bar before it makes the request, so the cancelled asset here is
// one that never got an answer at all.
func TestPullReturnsWhenAnUnreachableURLCancelsTheRest(t *testing.T) {
	manifestPath, lockPath := unreachableProject(t)
	asked := make(chan struct{})
	fetcher := &fakeFetcher{fetch: func(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		if request.URL == reachableURL {
			close(asked)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		// Fail once the other transfer is under way, so that what this cuts
		// short is a running transfer rather than one yet to start.
		<-asked
		return nil, errors.New("dial tcp: lookup nowhere.invalid: no such host")
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, terminalReporter())

	_, err := within(t, func() (application.PullResult, error) {
		return service.Pull(context.Background(), application.PullOptions{
			NetworkOptions: application.NetworkOptions{Concurrency: 2},
		})
	})
	if fault.As(err).Code != "network_error" {
		t.Fatalf("expected the unreachable origin to be reported, got %v", err)
	}
}

// Lock starts a bar only once an origin has answered, so the asset it leaves
// unfinished is one cancelled while its bytes were arriving.
func TestLockReturnsWhenAnUnreachableURLCancelsTheRest(t *testing.T) {
	manifestPath, lockPath := unreachableProject(t)
	body := &blockingBody{opened: make(chan struct{})}
	fetcher := &fakeFetcher{fetch: func(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		if request.URL == reachableURL {
			body.ctx = ctx
			return &application.FetchResponse{Length: 64, Body: body}, nil
		}
		// Fail once the other asset's bytes have started to arrive, which is
		// after its bar has been drawn.
		<-body.opened
		return nil, errors.New("dial tcp: lookup nowhere.invalid: no such host")
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, terminalReporter())

	_, err := within(t, func() (application.LockResult, error) {
		// Refresh, because the lock file this project carries already describes
		// both assets, and an entry a lock describes is never resolved again.
		return service.Lock(context.Background(), application.NetworkOptions{
			Concurrency: 2, MaxSize: 1024, Refresh: true,
		})
	})
	if fault.As(err).Code != "network_error" {
		t.Fatalf("expected the unreachable origin to be reported, got %v", err)
	}
}

const (
	reachableURL   = "https://example.com/reachable"
	unreachableURL = "https://nowhere.invalid/unreachable"
)

// unreachableProject writes a two asset project whose lock file describes both,
// one of them served from a host that does not resolve.
func unreachableProject(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("reachable@1"):   {URL: reachableURL},
			at("unreachable@1"): {URL: unreachableURL},
		},
	}
	locked := map[coord.Coordinate]project.LockAsset{}
	for name, asset := range manifest.Assets {
		content := []byte(name.Name)
		locked[name] = project.LockAsset{
			URL:      asset.URL,
			Digest:   digest.Bytes(content),
			Size:     int64(len(content)),
			Filename: name.Name,
		}
	}
	lock, err := project.NewLock(manifest, locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath
}

// terminalReporter builds the reporter a person at a terminal gets, on a
// context nothing cancels. The context is the point: an interrupt already ends
// this display, and what these tests are about is the run that ends without
// one. Where it draws is not, so it draws into a buffer.
func terminalReporter() application.Reporter {
	return progress.New(context.Background(), &bytes.Buffer{}, true, true, style.Palette{})
}

// blockingBody is an asset whose bytes never finish arriving. It reports the
// first read, which is the moment the transfer is visibly under way.
type blockingBody struct {
	ctx    context.Context
	once   sync.Once
	opened chan struct{}
}

func (body *blockingBody) Read([]byte) (int, error) {
	body.once.Do(func() { close(body.opened) })
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*blockingBody) Close() error { return nil }

// within runs an operation and fails the test if it has not returned in time.
// A reporter waiting for a bar nobody will finish does not deadlock the
// runtime -- its display goroutine is alive and drawing -- so nothing but a
// clock reports this.
func within[T any](t *testing.T, operation func() (T, error)) (T, error) {
	t.Helper()
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := operation()
		done <- outcome{value: value, err: err}
	}()
	select {
	case result := <-done:
		return result.value, result.err
	case <-time.After(30 * time.Second):
		t.Fatal("the operation never returned: a bar it started was never finished")
		panic("unreachable")
	}
}
