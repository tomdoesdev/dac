package trust

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// newGate returns a gate over a temporary file trusting the named hosts.
func newGate(t *testing.T, allowAll bool, hosts ...string) (*Gate, *Store) {
	t.Helper()
	store := testStore(t)
	list, err := store.Update(context.Background(), func(list List) (List, error) {
		updated, _ := list.Add(hosts, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		return updated, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return NewGate(store, list, allowAll), store
}

func request(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	value, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("NewRequest(%q): %v", rawURL, err)
	}
	return value
}

// send makes one request through a transport and closes whatever body comes back.
func send(t *testing.T, transport http.RoundTripper, rawURL string) error {
	t.Helper()
	response, err := transport.RoundTrip(request(t, rawURL))
	if err == nil {
		_ = response.Body.Close()
	}
	return err
}

// passThrough is a transport that answers every request it is given.
func passThrough(reached *atomic.Int32) http.RoundTripper {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		if reached != nil {
			reached.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
}

// TestAnUntrustedHostNeverReachesTheNetwork is the whole point of the feature.
// Refusing after the request went out would already have told the host that DAC
// wants the asset, so the check has to come before the transport under it.
func TestAnUntrustedHostNeverReachesTheNetwork(t *testing.T) {
	gate, _ := newGate(t, false, "trusted.example")
	var reached atomic.Int32
	transport := gate.Decorate(passThrough(&reached))

	err := send(t, transport, "https://untrusted.example/asset")
	if !errors.Is(err, application.ErrHostNotTrusted) {
		t.Fatalf("error is %v, want a refusal", err)
	}
	var refusal *application.HostError
	if !errors.As(err, &refusal) || refusal.Host != "untrusted.example" {
		t.Fatalf("error is %v, want one naming untrusted.example", err)
	}
	if reached.Load() != 0 {
		t.Fatalf("the transport ran %d times, want 0", reached.Load())
	}
}

func TestATrustedHostPassesThroughWhateverItsURLLooksLike(t *testing.T) {
	gate, _ := newGate(t, false, "trusted.example")
	var reached atomic.Int32
	transport := gate.Decorate(passThrough(&reached))

	// Case and port are not part of who is on the other end.
	for _, rawURL := range []string{"https://trusted.example/a", "https://Trusted.Example:8443/b"} {
		if err := send(t, transport, rawURL); err != nil {
			t.Fatalf("RoundTrip(%q): %v", rawURL, err)
		}
	}
	if reached.Load() != 2 {
		t.Fatalf("the transport ran %d times, want 2", reached.Load())
	}
}

func TestTrustAllLetsAnUntrustedHostThrough(t *testing.T) {
	gate, store := newGate(t, true)
	var reached atomic.Int32
	transport := gate.Decorate(passThrough(&reached))

	if err := send(t, transport, "https://untrusted.example/asset"); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if reached.Load() != 1 {
		t.Fatalf("the transport ran %d times, want 1", reached.Load())
	}
	// Skipping the check is not the same as trusting, so nothing is recorded.
	if err := gate.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	list, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list.Hosts) != 0 {
		t.Fatalf("hosts = %v, want none", list.Hosts)
	}
}

func TestFlushRecordsOnlyTheHostsTheRunUsed(t *testing.T) {
	gate, store := newGate(t, false, "used.example", "idle.example")
	transport := gate.Decorate(passThrough(nil))
	if err := send(t, transport, "https://used.example/asset"); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if err := gate.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	list, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if list.Hosts["used.example"].LastUsed.IsZero() {
		t.Fatal("the host the run downloaded from has no use time")
	}
	if !list.Hosts["idle.example"].LastUsed.IsZero() {
		t.Fatal("a host the run never reached was marked used")
	}
}

// TestFlushWritesNothingWhenTheRunDownloadedNothing keeps every offline command
// from rewriting the file for no reason.
func TestFlushWritesNothingWhenTheRunDownloadedNothing(t *testing.T) {
	gate, store := newGate(t, false, "example.com")
	// Removing the file makes any write at all visible, which a timestamp on a
	// filesystem with a coarse clock would not.
	if err := os.Remove(store.Path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := gate.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat = %v, want the file still gone", err)
	}
}

// TestFlushDoesNotTrustAHostRemovedWhileTheRunRan covers the window between the
// gate loading its list and the run finishing. A flush that wrote entries rather
// than touching existing ones would undo a dac trust remove from that window.
func TestFlushDoesNotTrustAHostRemovedWhileTheRunRan(t *testing.T) {
	gate, store := newGate(t, false, "example.com")
	transport := gate.Decorate(passThrough(nil))
	if err := send(t, transport, "https://example.com/asset"); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, err := store.Update(context.Background(), func(list List) (List, error) {
		updated, _ := list.Remove([]string{"example.com"})
		return updated, nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := gate.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	list, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list.Hosts) != 0 {
		t.Fatalf("hosts = %v, want none", list.Hosts)
	}
}

// TestTheGateHoldsUpUnderParallelRanges covers the transport the split download
// drives, where every range of one asset checks the same host at once.
func TestTheGateHoldsUpUnderParallelRanges(t *testing.T) {
	gate, store := newGate(t, false, "example.com")
	transport := gate.Decorate(passThrough(nil))
	// The requests are built here because a helper that fails does so on the test
	// goroutine, which is the only one allowed to end the test.
	requests := make([]*http.Request, 32)
	for index := range requests {
		target := "https://example.com/asset"
		if index%2 == 0 {
			target = "https://untrusted.example/asset"
		}
		requests[index] = request(t, target)
	}
	var group sync.WaitGroup
	for _, value := range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := transport.RoundTrip(value)
			if err == nil {
				_ = response.Body.Close()
			}
		}()
	}
	group.Wait()

	if err := gate.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	list, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if list.Hosts["example.com"].LastUsed.IsZero() {
		t.Fatal("no use was recorded")
	}
	if _, exists := list.Hosts["untrusted.example"]; exists {
		t.Fatal("a refused host was recorded")
	}
}

// TestTheGateRefusesThroughARealClient checks the decorator where it is used:
// wrapped around a transport by net/http rather than called directly.
func TestTheGateRefusesThroughARealClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	gate, _ := newGate(t, false, "trusted.example")
	client := &http.Client{Transport: gate.Decorate(http.DefaultTransport)}

	response, err := client.Get(server.URL)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("the client reached a host that is not trusted")
	}
	if !errors.Is(err, application.ErrHostNotTrusted) {
		t.Fatalf("error is %v, want a refusal", err)
	}
}
