package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestPackSplitsRawNARAndRewritesURL(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	outputDir := filepath.Join(root, "output")
	if err := os.MkdirAll(filepath.Join(cacheDir, "nar"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte("abcdefghij")
	if err := os.WriteFile(filepath.Join(cacheDir, "nar", "source.nar"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	narInfo := "StorePath: /nix/store/" + testHash + "-fixture\n" +
		"URL: nar/source.nar\n" +
		"Compression: none\n" +
		"FileHash: sha256:fixture\n" +
		"FileSize: 10\n" +
		"NarHash: sha256:fixture\n" +
		"NarSize: 10\n" +
		"References: \n" +
		"Sig: cache:test\n"
	if err := os.WriteFile(filepath.Join(cacheDir, testHash+".narinfo"), []byte(narInfo), 0o644); err != nil {
		t.Fatal(err)
	}
	added, skipped, err := Run(Options{
		CacheDir:     cacheDir,
		OutputDir:    outputDir,
		AssetBaseURL: "https://example.test/release",
		Prefix:       "test",
		PublicKey:    "cache:test",
		ChunkSize:    4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || skipped != 0 {
		t.Fatalf("got added=%d skipped=%d", added, skipped)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := cacheindex.Parse(data, "")
	if err != nil {
		t.Fatal(err)
	}
	entry := document.Paths[testHash]
	if len(entry.NAR.Extents) != 3 {
		t.Fatalf("got %d extents, want 3", len(entry.NAR.Extents))
	}
	var rebuilt []byte
	for _, extent := range entry.NAR.Extents {
		chunk, err := os.ReadFile(filepath.Join(outputDir, extent.Asset))
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, chunk[extent.Offset:extent.Offset+extent.Length]...)
	}
	if string(rebuilt) != string(raw) {
		t.Fatalf("rebuilt %q, want %q", rebuilt, raw)
	}
	if want := "URL: nar/" + testHash + ".nar\n"; !strings.Contains(entry.NarInfo, want) {
		t.Fatalf("rewritten narinfo does not contain %q", want)
	}
}
