package filter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

const (
	indexedHash  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	upstreamHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	selectedHash = "cccccccccccccccccccccccccccccccc"
)

func TestRunExcludesIndexedAndUpstreamPaths(t *testing.T) {
	root := t.TempDir()
	pathsFile := filepath.Join(root, "paths")
	output := filepath.Join(root, "selected")
	paths := strings.Join([]string{
		"/nix/store/" + indexedHash + "-indexed",
		"/nix/store/" + upstreamHash + "-upstream",
		"/nix/store/" + selectedHash + "-selected",
	}, "\n") + "\n"
	if err := os.WriteFile(pathsFile, []byte(paths), 0o644); err != nil {
		t.Fatal(err)
	}
	document := cacheindex.Empty("cache:test", time.Now().UTC().Format(time.RFC3339))
	document.Assets["chunk"] = cacheindex.Asset{URL: "https://example.test/chunk", Size: 1}
	document.Paths[indexedHash] = cacheindex.PathEntry{
		NarInfo: "URL: nar/" + indexedHash + ".nar\nCompression: none\nFileSize: 1\nNarSize: 1\nSig: cache:fixture\n",
		NAR: cacheindex.NAR{
			Size:    1,
			Extents: []cacheindex.Extent{{Asset: "chunk", Length: 1}},
		},
	}
	data, err := document.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(root, "index.json")
	if err := os.WriteFile(previous, data, 0o644); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, upstreamHash) {
			response.WriteHeader(http.StatusOK)
			return
		}
		response.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	result, err := Run(Options{
		PathsFile:     pathsFile,
		PreviousIndex: previous,
		CacheURL:      upstream.URL,
		Output:        output,
		Workers:       2,
		Timeout:       time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Selected != 1 || result.AlreadyIndexed != 1 || result.AvailableRemote != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	selected, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(selected) != "/nix/store/"+selectedHash+"-selected\n" {
		t.Fatalf("unexpected selection %q", selected)
	}
}
