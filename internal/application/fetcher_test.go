package application_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/httpclient"
)

type countingFetcher struct {
	next  application.Fetcher
	calls *atomic.Int32
}

// Fetch records one source-stage call and delegates the transfer.
func (fetcher countingFetcher) Fetch(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
	fetcher.calls.Add(1)
	return fetcher.next.Fetch(ctx, request)
}

func TestFetcherDecoratorRunsOnceForARedirectedAsset(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/source" {
			http.Redirect(writer, request, server.URL+"/asset", http.StatusFound)
			return
		}
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()

	client := httpclient.New(httpclient.Options{Timeout: time.Second})
	defer client.Close()
	var calls atomic.Int32
	decorated := countingFetcher{next: client, calls: &calls}
	response, err := decorated.Fetch(context.Background(), application.FetchRequest{
		URL:               server.URL + "/source",
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("decorator calls = %d, want 1", calls.Load())
	}
}
