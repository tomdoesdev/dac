package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testOrigin builds an unthrottled origin whose logs do not reach test output.
func testOrigin(name string, size byteCount, seed int64) *origin {
	return newOrigin(name, assetConfig{Size: size, Seed: seed}, 0, log.New(io.Discard, "", 0))
}

// requestOrigin records one synthetic response without opening a network listener.
func requestOrigin(t *testing.T, origin *origin, method string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1/"+origin.name, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	origin.serve(response, request)
	return response
}

// TestOriginValidatorsAreDeterministicAndFollowContent protects the synthetic
// validators from drifting with process time or staying fixed across byte changes.
func TestOriginValidatorsAreDeterministicAndFollowContent(t *testing.T) {
	first := testOrigin("asset.bin", 64, 7)
	same := testOrigin("asset.bin", 64, 7)
	changedSeed := testOrigin("asset.bin", 64, 8)
	changedSize := testOrigin("asset.bin", 65, 7)

	if first.etag != same.etag || !first.lastModified.Equal(same.lastModified) {
		t.Fatalf("identical assets have different validators: %#v %#v", first, same)
	}
	if strings.HasPrefix(first.etag, "W/") || !strings.HasPrefix(first.etag, `"`) || !strings.HasSuffix(first.etag, `"`) {
		t.Fatalf("ETag %q is not a quoted strong validator", first.etag)
	}
	for name, changed := range map[string]*origin{"seed": changedSeed, "size": changedSize} {
		if changed.etag == first.etag {
			t.Errorf("changing %s did not change the ETag", name)
		}
		if changed.lastModified.Equal(first.lastModified) {
			t.Errorf("changing %s did not change Last-Modified", name)
		}
	}
	if first.lastModified.Nanosecond() != 0 || first.lastModified.Before(time.Unix(syntheticModifiedEpoch, 0)) ||
		!first.lastModified.Before(time.Unix(syntheticModifiedEpoch+int64(syntheticModifiedWindow), 0)) {
		t.Fatalf("generated modification time is outside the historical whole-second window: %s", first.lastModified)
	}
}

// TestOriginResponsesCarryValidators covers both request methods ServeContent
// accepts without duplicating the generated body for HEAD.
func TestOriginResponsesCarryValidators(t *testing.T) {
	origin := testOrigin("asset.bin", 64, 7)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			response := requestOrigin(t, origin, method, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("ETag"); got != origin.etag {
				t.Fatalf("ETag = %q, want %q", got, origin.etag)
			}
			modified := response.Header().Get("Last-Modified")
			parsed, err := http.ParseTime(modified)
			if err != nil || !parsed.Equal(origin.lastModified) {
				t.Fatalf("Last-Modified = %q (%v), want %s", modified, err, origin.lastModified)
			}
			if got := response.Header().Get("Content-Length"); got != strconv.FormatInt(origin.size, 10) {
				t.Fatalf("Content-Length = %q, want %d", got, origin.size)
			}
			if method == http.MethodHead && response.Body.Len() != 0 {
				t.Fatalf("HEAD wrote %d body bytes", response.Body.Len())
			}
		})
	}
}

// TestOriginHonorsConditionalRequests proves both validators can independently
// suppress an unchanged body without suppressing content for a stale value.
func TestOriginHonorsConditionalRequests(t *testing.T) {
	origin := testOrigin("asset.bin", 64, 7)
	modified := origin.lastModified.Format(http.TimeFormat)

	for name, headers := range map[string]map[string]string{
		"etag":          {"If-None-Match": origin.etag},
		"last modified": {"If-Modified-Since": modified},
	} {
		t.Run(name, func(t *testing.T) {
			response := requestOrigin(t, origin, http.MethodGet, headers)
			if response.Code != http.StatusNotModified || response.Body.Len() != 0 {
				t.Fatalf("conditional response = %d with %d bytes, want 304 with no body", response.Code, response.Body.Len())
			}
		})
	}

	for name, headers := range map[string]map[string]string{
		"etag":          {"If-None-Match": `"stale"`},
		"last modified": {"If-Modified-Since": origin.lastModified.Add(-time.Second).Format(http.TimeFormat)},
	} {
		t.Run("stale "+name, func(t *testing.T) {
			response := requestOrigin(t, origin, http.MethodGet, headers)
			if response.Code != http.StatusOK || int64(response.Body.Len()) != origin.size {
				t.Fatalf("stale conditional response = %d with %d bytes, want 200 with %d bytes", response.Code, response.Body.Len(), origin.size)
			}
		})
	}
}

// TestOriginServesDateValidatedRanges exercises the Last-Modified path used
// when a ranged downloader protects its parts with a date instead of an ETag.
func TestOriginServesDateValidatedRanges(t *testing.T) {
	origin := testOrigin("asset.bin", 64, 7)
	full := requestOrigin(t, origin, http.MethodGet, nil)
	ranged := requestOrigin(t, origin, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Range": origin.lastModified.Format(http.TimeFormat),
	})

	if ranged.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want %d", ranged.Code, http.StatusPartialContent)
	}
	if got := ranged.Header().Get("Content-Range"); got != "bytes 2-5/64" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got, want := ranged.Body.String(), full.Body.String()[2:6]; got != want {
		t.Fatalf("range bytes differ: got %x want %x", got, want)
	}
}
