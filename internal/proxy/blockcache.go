package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

type BlockCache struct {
	directory string
	blockSize int64
	maxSize   int64
	client    *http.Client
	downloads chan struct{}

	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func NewBlockCache(directory string, blockSize, maxSize int64, maxDownloads int, client *http.Client) (*BlockCache, error) {
	if blockSize <= 0 {
		return nil, errors.New("block size must be positive")
	}
	if maxDownloads <= 0 {
		return nil, errors.New("maximum downloads must be positive")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create block cache: %w", err)
	}
	return &BlockCache{
		directory: directory,
		blockSize: blockSize,
		maxSize:   maxSize,
		client:    client,
		downloads: make(chan struct{}, maxDownloads),
		inflight:  make(map[string]chan struct{}),
	}, nil
}

func (cache *BlockCache) Acquire(ctx context.Context, assetName string, asset cacheindex.Asset, blockNumber int64) (*os.File, error) {
	start := blockNumber * cache.blockSize
	if start < 0 || start >= asset.Size {
		return nil, fmt.Errorf("block %d is outside asset %s", blockNumber, assetName)
	}
	expected := min(cache.blockSize, asset.Size-start)
	identity := fmt.Sprintf("%s\x00%s\x00%d", assetName, asset.URL, asset.Size)
	key := fmt.Sprintf("%x-%08d", sha256.Sum256([]byte(identity)), blockNumber)
	path := filepath.Join(cache.directory, key+".block")

	for {
		cache.mu.Lock()
		if file, err := openValidBlock(path, expected); err == nil {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			cache.mu.Unlock()
			return file, nil
		}
		if completed, exists := cache.inflight[key]; exists {
			cache.mu.Unlock()
			select {
			case <-completed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		completed := make(chan struct{})
		cache.inflight[key] = completed
		cache.mu.Unlock()

		err := cache.download(asset.URL, path, start, expected, asset.Size)

		cache.mu.Lock()
		delete(cache.inflight, key)
		close(completed)
		if err != nil {
			cache.mu.Unlock()
			return nil, err
		}
		file, openErr := openValidBlock(path, expected)
		cache.mu.Unlock()
		if openErr != nil {
			return nil, openErr
		}
		cache.prune()
		return file, nil
	}
}

func openValidBlock(path string, expected int64) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != expected {
		file.Close()
		if err == nil {
			err = fmt.Errorf("cached block has size %d, want %d", info.Size(), expected)
		}
		return nil, err
	}
	return file, nil
}

func (cache *BlockCache) download(url, path string, start, expected, assetSize int64) error {
	cache.downloads <- struct{}{}
	defer func() { <-cache.downloads }()
	partial := path + ".partial"
	var existing int64
	if info, err := os.Stat(partial); err == nil {
		existing = info.Size()
		if existing > expected {
			if err := os.Remove(partial); err != nil {
				return fmt.Errorf("remove oversized partial block: %w", err)
			}
			existing = 0
		}
	}
	if existing == expected {
		return os.Rename(partial, path)
	}
	rangeStart := start + existing
	rangeEnd := start + expected - 1
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create range request: %w", err)
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rangeStart, rangeEnd))
	request.Header.Set("User-Agent", "nix-cache-proxy/1")
	response, err := cache.client.Do(request)
	if err != nil {
		return fmt.Errorf("download range: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("upstream ignored range: status %d", response.StatusCode)
	}
	if err := validateContentRange(response.Header.Get("Content-Range"), rangeStart, rangeEnd, assetSize); err != nil {
		return err
	}
	wanted := expected - existing
	if response.ContentLength != wanted {
		return fmt.Errorf("upstream Content-Length is %d, want %d", response.ContentLength, wanted)
	}
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open partial block: %w", err)
	}
	written, copyErr := io.CopyN(file, response.Body, wanted)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download block after %d bytes: %w", written, copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync block: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close block: %w", closeErr)
	}
	if err := os.Rename(partial, path); err != nil {
		return fmt.Errorf("publish block: %w", err)
	}
	return nil
}

func validateContentRange(value string, start, end, totalSize int64) error {
	prefix, total, found := strings.Cut(value, "/")
	if !found || total == "" {
		return fmt.Errorf("invalid Content-Range %q", value)
	}
	prefix = strings.TrimPrefix(prefix, "bytes ")
	actualStart, actualEnd, found := strings.Cut(prefix, "-")
	if !found {
		return fmt.Errorf("invalid Content-Range %q", value)
	}
	parsedStart, startErr := strconv.ParseInt(actualStart, 10, 64)
	parsedEnd, endErr := strconv.ParseInt(actualEnd, 10, 64)
	parsedTotal, totalErr := strconv.ParseInt(total, 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil || parsedStart != start || parsedEnd != end || parsedTotal != totalSize {
		return fmt.Errorf("Content-Range %q does not match bytes %d-%d", value, start, end)
	}
	return nil
}

type cachedFile struct {
	path    string
	size    int64
	modTime time.Time
}

func (cache *BlockCache) prune() {
	if cache.maxSize <= 0 {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		return
	}
	var files []cachedFile
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".block") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, cachedFile{
			path:    filepath.Join(cache.directory, entry.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		total += info.Size()
	}
	sort.Slice(files, func(left, right int) bool { return files[left].modTime.Before(files[right].modTime) })
	for _, file := range files {
		if total <= cache.maxSize {
			break
		}
		if os.Remove(file.path) == nil {
			total -= file.size
		}
	}
}
