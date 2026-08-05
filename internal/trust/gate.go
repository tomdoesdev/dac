package trust

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
)

// Gate refuses requests to hosts the trusted-hosts file does not name.
// It is the transport stage of the download pipeline, which is the one place
// that sees every request DAC makes: the asset, each redirect it follows, and
// each range of a split download.
type Gate struct {
	store *Store
	// hosts is settled before the first request, so reading it needs no lock.
	hosts    map[string]struct{}
	allowAll bool

	mutex sync.Mutex
	used  map[string]struct{}
}

// NewGate builds the refusal for one run from the list it loaded.
func NewGate(store *Store, list List, allowAll bool) *Gate {
	hosts := make(map[string]struct{}, len(list.Hosts))
	for host := range list.Hosts {
		hosts[host] = struct{}{}
	}
	return &Gate{store: store, hosts: hosts, allowAll: allowAll, used: map[string]struct{}{}}
}

// Decorate returns the transport stage that applies the trust rule.
// The result is an httpclient.TransportDecorator without naming that type, which
// keeps the trust list out of the dependencies of the client that enforces it.
func (gate *Gate) Decorate(next http.RoundTripper) http.RoundTripper {
	return &guard{gate: gate, next: next}
}

// Flush records the hosts this run downloaded from.
// Use times are collected in memory and written once, because a request-time
// write would mean one file rewrite per range of every split download.
func (gate *Gate) Flush(ctx context.Context) error {
	used := gate.drain()
	if len(used) == 0 {
		return nil
	}
	now := time.Now().UTC()
	_, err := gate.store.Update(ctx, func(list List) (List, error) {
		return list.Touch(used, now), nil
	})
	return err
}

// drain takes the hosts recorded so far.
func (gate *Gate) drain() []string {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	used := slices.Collect(maps.Keys(gate.used))
	clear(gate.used)
	slices.Sort(used)
	return used
}

// check reports whether a request may go out, and counts a trusted host as used.
// A host that only --insecure-trust-all let through is not recorded: a use time
// says when an entry was last worth keeping, and there is no entry.
func (gate *Gate) check(host string) bool {
	_, trusted := gate.hosts[host]
	if trusted {
		gate.mutex.Lock()
		gate.used[host] = struct{}{}
		gate.mutex.Unlock()
	}
	return trusted || gate.allowAll
}

// guard is the round tripper the gate wraps around the transport under it.
type guard struct {
	gate *Gate
	next http.RoundTripper
}

// RoundTrip refuses an untrusted host before the request reaches the network.
func (transport *guard) RoundTrip(request *http.Request) (*http.Response, error) {
	host := Normalize(request.URL.Hostname())
	if !transport.gate.check(host) {
		return nil, &application.HostError{Host: host}
	}
	return transport.next.RoundTrip(request)
}
