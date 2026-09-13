package cacheindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const Version = 1

type Document struct {
	Version     int                  `json:"version"`
	GeneratedAt string               `json:"generated_at"`
	StoreDir    string               `json:"store_dir"`
	PublicKey   string               `json:"public_key"`
	Assets      map[string]Asset     `json:"assets"`
	Paths       map[string]PathEntry `json:"paths"`
}

type Asset struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type PathEntry struct {
	NarInfo string `json:"narinfo"`
	NAR     NAR    `json:"nar"`
}

type NAR struct {
	Size     int64    `json:"size"`
	FileHash string   `json:"file_hash"`
	Extents  []Extent `json:"extents"`
}

type Extent struct {
	Asset  string `json:"asset"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

func Empty(publicKey, generatedAt string) *Document {
	return &Document{
		Version:     Version,
		GeneratedAt: generatedAt,
		StoreDir:    "/nix/store",
		PublicKey:   publicKey,
		Assets:      make(map[string]Asset),
		Paths:       make(map[string]PathEntry),
	}
}

func Parse(data []byte, assetURLPrefix string) (*Document, error) {
	var document Document
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}
	if err := document.Validate(assetURLPrefix); err != nil {
		return nil, err
	}
	return &document, nil
}

func (document *Document) Validate(assetURLPrefix string) error {
	if document.Version != Version {
		return fmt.Errorf("unsupported index version %d", document.Version)
	}
	if document.StoreDir != "/nix/store" {
		return errors.New("index StoreDir must be /nix/store")
	}
	keyName, _, found := strings.Cut(document.PublicKey, ":")
	if !found || keyName == "" {
		return errors.New("index public key is invalid")
	}
	if document.Assets == nil || document.Paths == nil {
		return errors.New("assets and paths must be objects")
	}
	for name, asset := range document.Assets {
		if name == "" || asset.URL == "" || asset.Size < 0 {
			return fmt.Errorf("invalid asset %q", name)
		}
		if assetURLPrefix != "" && !strings.HasPrefix(asset.URL, assetURLPrefix) {
			return fmt.Errorf("asset %q URL is outside the allowed prefix", name)
		}
	}
	for storeHash, path := range document.Paths {
		if !ValidStoreHash(storeHash) {
			return fmt.Errorf("invalid store hash %q", storeHash)
		}
		if !strings.Contains(path.NarInfo, "URL: nar/"+storeHash+".nar\n") {
			return fmt.Errorf("narinfo URL mismatch for %s", storeHash)
		}
		if !strings.Contains(path.NarInfo, "Compression: none\n") {
			return fmt.Errorf("compressed NAR is not supported for %s", storeHash)
		}
		if !strings.Contains(path.NarInfo, "Sig: "+keyName+":") {
			return fmt.Errorf("narinfo is not signed by %s for %s", keyName, storeHash)
		}
		if path.NAR.Size < 0 || len(path.NAR.Extents) == 0 {
			return fmt.Errorf("invalid NAR metadata for %s", storeHash)
		}
		if !strings.Contains(path.NarInfo, fmt.Sprintf("FileSize: %d\n", path.NAR.Size)) ||
			!strings.Contains(path.NarInfo, fmt.Sprintf("NarSize: %d\n", path.NAR.Size)) {
			return fmt.Errorf("narinfo size mismatch for %s", storeHash)
		}
		var total int64
		for _, extent := range path.NAR.Extents {
			asset, exists := document.Assets[extent.Asset]
			if !exists {
				return fmt.Errorf("unknown asset %q for %s", extent.Asset, storeHash)
			}
			if extent.Offset < 0 || extent.Length <= 0 || extent.Offset+extent.Length > asset.Size {
				return fmt.Errorf("invalid extent in asset %q for %s", extent.Asset, storeHash)
			}
			total += extent.Length
		}
		if total != path.NAR.Size {
			return fmt.Errorf("extent size mismatch for %s: got %d, want %d", storeHash, total, path.NAR.Size)
		}
	}
	return nil
}

func ValidStoreHash(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdfghijklmnpqrsvwxyz", character) {
			return false
		}
	}
	return true
}

func (document *Document) Marshal() ([]byte, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode index: %w", err)
	}
	return append(data, '\n'), nil
}
