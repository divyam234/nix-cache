package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nixcache/internal/cacheindex"
)

func indexJSON(t *testing.T, key, generated string) []byte {
	t.Helper()
	data, err := json.Marshal(cacheindex.Empty(key, generated))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func refreshServer(t *testing.T, handler http.HandlerFunc) (*Server, string) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cacheFile := filepath.Join(t.TempDir(), "index.json")
	server, err := New(Config{
		IndexURL: upstream.URL, IndexCache: cacheFile, PublicKey: "test:key",
		MaxDownloads: 1, RefreshEvery: time.Hour, HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, cacheFile
}

func TestRefreshEndpointPublishesAndReportsIndex(t *testing.T) {
	old := indexJSON(t, "test:key", "old")
	updated := indexJSON(t, "test:key", "2026-10-31T12:00:00Z")
	var body atomic.Value
	body.Store(old)
	var requests atomic.Int64
	server, cacheFile := refreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body.Load().([]byte))
	})
	body.Store(updated)
	endpoint := httptest.NewServer(server)
	defer endpoint.Close()
	for range 2 {
		response, err := http.Post(endpoint.URL+"/-/refresh", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var result RefreshResult
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, decode=%v", response.StatusCode, err)
		}
		if result.SHA256 != fmt.Sprintf("%x", sha256.Sum256(updated)) || result.GeneratedAt != "2026-10-31T12:00:00Z" || result.Paths != 0 {
			t.Fatalf("wrong refresh result: %+v", result)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("refresh response must not be cached")
		}
		if server.index.get().GeneratedAt != result.GeneratedAt {
			t.Fatal("response does not describe the active index")
		}
		saved, err := os.ReadFile(cacheFile)
		if err != nil || string(saved) != string(updated) {
			t.Fatalf("persistent index not updated: %s, %v", saved, err)
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("expected startup plus two fresh downloads, got %d", requests.Load())
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(method, "/-/refresh", nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s accepted: %d", method, recorder.Code)
		}
	}
	if requests.Load() != 3 {
		t.Fatal("non-POST request triggered a refresh")
	}
}

func TestFailedRefreshRetainsCurrentAndPersistentIndex(t *testing.T) {
	for _, failure := range []string{"upstream", "invalid-json", "wrong-key", "disk", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			old := indexJSON(t, "test:key", "old")
			updated := indexJSON(t, "test:key", "new")
			wrongKey := indexJSON(t, "other:key", "new")
			var ready atomic.Bool
			server, cacheFile := refreshServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !ready.Load() {
					_, _ = w.Write(old)
					return
				}
				switch failure {
				case "upstream":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case "invalid-json":
					_, _ = io.WriteString(w, "not json")
				case "wrong-key":
					_, _ = w.Write(wrongKey)
				default:
					_, _ = w.Write(updated)
				}
			})
			ready.Store(true)
			if failure == "disk" {
				server.index.cacheFile = filepath.Join(cacheFile, "cannot-write-here")
			}
			request := httptest.NewRequest(http.MethodPost, "/-/refresh", nil)
			if failure == "cancelled" {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "retaining current index") {
				t.Fatalf("failure hidden: %d %s", recorder.Code, recorder.Body.String())
			}
			if server.index.get().GeneratedAt != "old" {
				t.Fatal("failed refresh changed the active index")
			}
			saved, err := os.ReadFile(cacheFile)
			if err != nil || string(saved) != string(old) {
				t.Fatalf("failed refresh changed saved index: %s, %v", saved, err)
			}
		})
	}
}

func TestConcurrentManualAndScheduledRefreshes(t *testing.T) {
	var downloads atomic.Int64
	server, cacheFile := refreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		document := cacheindex.Empty("test:key", fmt.Sprint(downloads.Add(1)))
		_ = json.NewEncoder(w).Encode(document)
	})
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			if i%2 == 0 {
				if _, err := server.index.refresh(context.Background()); err != nil {
					t.Error(err)
				}
			} else {
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/-/refresh", nil))
				if recorder.Code != http.StatusOK {
					t.Errorf("refresh failed: %s", recorder.Body.String())
				}
			}
		})
	}
	workers.Wait()
	saved, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatal(err)
	}
	var document cacheindex.Document
	if err := json.Unmarshal(saved, &document); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 9 || document.GeneratedAt != "9" || server.index.get().GeneratedAt != "9" {
		t.Fatalf("refreshes published out of order: downloads=%d, saved=%s, active=%s", downloads.Load(), document.GeneratedAt, server.index.get().GeneratedAt)
	}
}
