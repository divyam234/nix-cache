package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nixcache/internal/cacheindex"
)

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRangeRedirectCacheAndNAR(t *testing.T) {
	var redirects, downloads atomic.Int64
	data := []byte("abcdefghij")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		startText, endText, _ := strings.Cut(value, "-")
		start, _ := strconv.Atoi(startText)
		end, _ := strconv.Atoi(endText)
		body := data[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer cdn.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		http.Redirect(w, r, cdn.URL+"?se="+time.Now().Add(time.Minute).UTC().Format(time.RFC3339), http.StatusFound)
	}))
	defer origin.Close()
	source, err := newRangeSource(origin.Client(), 2)
	if err != nil {
		t.Fatal(err)
	}
	document := &cacheindex.Document{
		Assets: map[string]cacheindex.Asset{"one": {URL: origin.URL, Size: 10}},
		Paths:  map[string]cacheindex.PathEntry{testHash: {NAR: cacheindex.NAR{Size: 6, Extents: []cacheindex.Extent{{Asset: "one", Offset: 2, Length: 3}, {Asset: "one", Offset: 6, Length: 3}}}}},
	}
	server := httptest.NewServer(&Server{index: &indexStore{current: document}, ranges: source})
	defer server.Close()
	for range 2 {
		response, err := http.Get(server.URL + "/nar/" + testHash + ".nar")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || string(body) != "cdeghi" {
			t.Fatalf("status %d body %q error %v", response.StatusCode, body, err)
		}
	}
	if redirects.Load() != 1 || downloads.Load() != 4 {
		t.Fatalf("redirects %d downloads %d", redirects.Load(), downloads.Load())
	}
}

func TestExpiredRedirectRefreshes(t *testing.T) {
	source, _ := newRangeSource(http.DefaultClient, 1)
	source.redirects["origin"] = redirect{url: "cdn", expires: time.Now().Add(10 * time.Second)}
	if source.cachedURL("origin") != "origin" {
		t.Fatal("near-expired URL reused")
	}
}

func TestIgnoredRangeRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("abc")) }))
	defer upstream.Close()
	source, _ := newRangeSource(upstream.Client(), 1)
	_, err := source.open(context.Background(), upstream.URL, 0, 2, 3)
	if err == nil {
		t.Fatal("expected range rejection")
	}
}

func TestRejectedCachedRedirectRefreshes(t *testing.T) {
	var redirects atomic.Int64
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer stale.Close()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 1-2/3")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("bc"))
	}))
	defer cdn.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		http.Redirect(w, r, cdn.URL+"?se="+time.Now().Add(time.Minute).UTC().Format(time.RFC3339), http.StatusFound)
	}))
	defer origin.Close()
	source, _ := newRangeSource(origin.Client(), 1)
	source.redirects[origin.URL] = redirect{url: stale.URL, expires: time.Now().Add(time.Minute)}
	var output strings.Builder
	if _, err := source.copy(context.Background(), &output, origin.URL, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if output.String() != "bc" || redirects.Load() != 1 {
		t.Fatalf("body %q redirects %d", output.String(), redirects.Load())
	}
}
