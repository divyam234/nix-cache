package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func rangeServer(t *testing.T, data []byte, calls *atomic.Int64, ignoreRange bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if ignoreRange {
			response.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = response.Write(data)
			return
		}
		value := strings.TrimPrefix(request.Header.Get("Range"), "bytes=")
		startText, endText, found := strings.Cut(value, "-")
		if !found {
			t.Errorf("invalid Range %q", value)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		start, _ := strconv.Atoi(startText)
		end, _ := strconv.Atoi(endText)
		body := data[start : end+1]
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		response.Header().Set("Content-Length", strconv.Itoa(len(body)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(body)
	}))
}

func TestBlockCacheCoalescesAndReusesDownloads(t *testing.T) {
	var calls atomic.Int64
	upstream := rangeServer(t, []byte("abcdefgh"), &calls, false)
	defer upstream.Close()
	cache, err := NewBlockCache(t.TempDir(), 4, 1024, 2, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	asset := cacheindex.Asset{URL: upstream.URL, Size: 8}
	first, err := cache.Acquire(context.Background(), "asset", asset, 0)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := cache.Acquire(context.Background(), "asset", asset, 0)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("got %d upstream calls, want 1", got)
	}
}

func TestServerReconstructsNARAcrossAssetsAndBlocks(t *testing.T) {
	var firstCalls, secondCalls atomic.Int64
	first := rangeServer(t, []byte("abcde"), &firstCalls, false)
	defer first.Close()
	second := rangeServer(t, []byte("fghij"), &secondCalls, false)
	defer second.Close()
	blocks, err := NewBlockCache(t.TempDir(), 4, 1024, 2, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	document := testDocument(first.URL, second.URL)
	server := &Server{
		config: Config{BlockSize: 4},
		index:  &indexStore{current: document},
		blocks: blocks,
	}
	endpoint := httptest.NewServer(server)
	defer endpoint.Close()

	response, err := http.Get(endpoint.URL + "/nar/" + testHash + ".nar")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "cdefgh" {
		t.Fatalf("got status %d body %q", response.StatusCode, body)
	}

	response, err = http.Get(endpoint.URL + "/nar/" + testHash + ".nar")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if firstCalls.Load() != 2 || secondCalls.Load() != 1 {
		t.Fatalf("unexpected upstream calls: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
}

func TestServerPrefetchesNextBlockWhileWritingCurrentBlock(t *testing.T) {
	ranges := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		value := strings.TrimPrefix(request.Header.Get("Range"), "bytes=")
		ranges <- value
		startText, endText, _ := strings.Cut(value, "-")
		start, _ := strconv.Atoi(startText)
		end, _ := strconv.Atoi(endText)
		data := []byte("abcdefgh")
		body := data[start : end+1]
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		response.Header().Set("Content-Length", strconv.Itoa(len(body)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(body)
	}))
	defer upstream.Close()

	blocks, err := NewBlockCache(t.TempDir(), 4, 1024, 2, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	document := &cacheindex.Document{
		Assets: map[string]cacheindex.Asset{"one": {URL: upstream.URL, Size: 8}},
		Paths: map[string]cacheindex.PathEntry{
			testHash: {NAR: cacheindex.NAR{Size: 8, Extents: []cacheindex.Extent{{Asset: "one", Length: 8}}}},
		},
	}
	server := &Server{config: Config{BlockSize: 4}, index: &indexStore{current: document}, blocks: blocks}
	releaseWrite := make(chan struct{})
	defer func() {
		select {
		case <-releaseWrite:
		default:
			close(releaseWrite)
		}
	}()
	writer := &blockingResponseWriter{header: make(http.Header), writing: make(chan struct{}), release: releaseWrite}
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/nar/"+testHash+".nar", nil))
		close(done)
	}()

	if first := receive(t, ranges); first != "0-3" {
		t.Fatalf("first range is %q, want 0-3", first)
	}
	receive(t, writer.writing)
	if second := receive(t, ranges); second != "4-7" {
		t.Fatalf("prefetched range is %q, want 4-7", second)
	}
	close(releaseWrite)
	receive(t, done)
	if got := writer.body.String(); got != "abcdefgh" {
		t.Fatalf("got body %q, want abcdefgh", got)
	}
}

func TestServerRejectsIgnoredRange(t *testing.T) {
	var calls atomic.Int64
	upstream := rangeServer(t, []byte("abcde"), &calls, true)
	defer upstream.Close()
	blocks, err := NewBlockCache(t.TempDir(), 4, 1024, 2, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	document := testDocument(upstream.URL, upstream.URL)
	server := &Server{config: Config{BlockSize: 4}, index: &indexStore{current: document}, blocks: blocks}
	request := httptest.NewRequest(http.MethodGet, "/nar/"+testHash+".nar", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("got status %d, want 502", recorder.Code)
	}
}

func testDocument(firstURL, secondURL string) *cacheindex.Document {
	return &cacheindex.Document{
		Version:   cacheindex.Version,
		StoreDir:  "/nix/store",
		PublicKey: "cache:test",
		Assets: map[string]cacheindex.Asset{
			"one": {URL: firstURL, Size: 5},
			"two": {URL: secondURL, Size: 5},
		},
		Paths: map[string]cacheindex.PathEntry{
			testHash: {
				NarInfo: "URL: nar/" + testHash + ".nar\nCompression: none\nFileSize: 6\nNarSize: 6\nSig: cache:fixture\n",
				NAR: cacheindex.NAR{
					Size: 6,
					Extents: []cacheindex.Extent{
						{Asset: "one", Offset: 2, Length: 3},
						{Asset: "two", Offset: 0, Length: 3},
					},
				},
			},
		},
	}
}

type blockingResponseWriter struct {
	header  http.Header
	body    strings.Builder
	writing chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (writer *blockingResponseWriter) Header() http.Header {
	return writer.header
}

func (writer *blockingResponseWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() {
		close(writer.writing)
		<-writer.release
	})
	return writer.body.Write(data)
}

func (writer *blockingResponseWriter) WriteHeader(_ int) {}

func receive[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for value")
		var zero T
		return zero
	}
}
