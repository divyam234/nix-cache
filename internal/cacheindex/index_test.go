package cacheindex

import "testing"

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validDocument() *Document {
	return &Document{
		Version:   Version,
		StoreDir:  "/nix/store",
		PublicKey: "cache:test",
		Assets: map[string]Asset{
			"chunk": {URL: "https://example.test/chunk", Size: 10},
		},
		Paths: map[string]PathEntry{
			testHash: {
				NarInfo: "URL: nar/" + testHash + ".nar\nCompression: none\nFileSize: 6\nNarSize: 6\nSig: cache:fixture\n",
				NAR: NAR{
					Size:    6,
					Extents: []Extent{{Asset: "chunk", Offset: 2, Length: 6}},
				},
			},
		},
	}
}

func TestValidateRejectsOutOfBoundsExtent(t *testing.T) {
	document := validDocument()
	entry := document.Paths[testHash]
	entry.NAR.Extents[0].Length = 9
	document.Paths[testHash] = entry
	if err := document.Validate(""); err == nil {
		t.Fatal("expected out-of-bounds extent to fail validation")
	}
}

func TestValidateRestrictsAssetURLs(t *testing.T) {
	document := validDocument()
	if err := document.Validate("https://github.com/divyam234/nix-cache/releases/download/"); err == nil {
		t.Fatal("expected URL outside prefix to fail validation")
	}
}
