package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

var (
	narInfoPath = regexp.MustCompile(`^/([0-9abcdfghijklmnpqrsvwxyz]{32})\.narinfo$`)
	narPath     = regexp.MustCompile(`^/nar/([0-9abcdfghijklmnpqrsvwxyz]{32})\.nar$`)
)

type Config struct {
	Listen         string
	IndexURL       string
	IndexCache     string
	AssetURLPrefix string
	PublicKey      string
	BlockDirectory string
	BlockSize      int64
	MaxCacheSize   int64
	MaxDownloads   int
	RefreshEvery   time.Duration
	HTTPTimeout    time.Duration
}

type indexStore struct {
	url         string
	cacheFile   string
	assetPrefix string
	publicKey   string
	client      *http.Client
	mu          sync.RWMutex
	current     *cacheindex.Document
}

type Server struct {
	config Config
	index  *indexStore
	blocks *BlockCache
	http   *http.Server
}

func New(config Config) (*Server, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 16
	transport.IdleConnTimeout = 90 * time.Second
	client := &http.Client{Transport: transport, Timeout: config.HTTPTimeout}
	blocks, err := NewBlockCache(config.BlockDirectory, config.BlockSize, config.MaxCacheSize, config.MaxDownloads, client)
	if err != nil {
		return nil, err
	}
	store := &indexStore{
		url:         config.IndexURL,
		cacheFile:   config.IndexCache,
		assetPrefix: config.AssetURLPrefix,
		publicKey:   config.PublicKey,
		client:      client,
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	server := &Server{config: config, index: store, blocks: blocks}
	server.http = &http.Server{
		Addr:              config.Listen,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return server, nil
}

func (server *Server) ListenAndServe(ctx context.Context) error {
	go server.refresh(ctx)
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.http.Shutdown(shutdownContext)
	}()
	err := server.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/nix-cache-info":
		writeBytes(response, request, "text/x-nix-cache-info", []byte("StoreDir: /nix/store\nWantMassQuery: 1\nPriority: 30\n"))
	case request.URL.Path == "/healthz":
		writeBytes(response, request, "text/plain", []byte("ok\n"))
	case narInfoPath.MatchString(request.URL.Path):
		storeHash := narInfoPath.FindStringSubmatch(request.URL.Path)[1]
		path, exists := server.index.get().Paths[storeHash]
		if !exists {
			http.NotFound(response, request)
			return
		}
		writeBytes(response, request, "text/x-nix-narinfo", []byte(path.NarInfo))
	case narPath.MatchString(request.URL.Path):
		storeHash := narPath.FindStringSubmatch(request.URL.Path)[1]
		document := server.index.get()
		path, exists := document.Paths[storeHash]
		if !exists {
			http.NotFound(response, request)
			return
		}
		server.serveNAR(response, request, document, path)
	default:
		http.NotFound(response, request)
	}
}

func (server *Server) serveNAR(response http.ResponseWriter, request *http.Request, document *cacheindex.Document, path cacheindex.PathEntry) {
	response.Header().Set("Content-Type", "application/x-nix-nar")
	response.Header().Set("Content-Length", fmt.Sprint(path.NAR.Size))
	if request.Method == http.MethodHead {
		return
	}
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	first := true
	for extentIndex, extent := range path.NAR.Extents {
		asset := document.Assets[extent.Asset]
		remaining := extent.Length
		position := extent.Offset
		for remaining > 0 {
			blockNumber := position / server.config.BlockSize
			offsetInBlock := position % server.config.BlockSize
			length := min(remaining, server.config.BlockSize-offsetInBlock)
			block, err := server.blocks.Acquire(request.Context(), extent.Asset, asset, blockNumber)
			if err != nil {
				if first {
					response.Header().Del("Content-Length")
					http.Error(response, err.Error(), http.StatusBadGateway)
				} else {
					log.Printf("stream %s failed: %v", request.URL.Path, err)
				}
				return
			}
			if remaining > length {
				nextPosition := position + length
				server.blocks.Prefetch(extent.Asset, asset, nextPosition/server.config.BlockSize)
			} else if extentIndex+1 < len(path.NAR.Extents) {
				nextExtent := path.NAR.Extents[extentIndex+1]
				nextBlock := nextExtent.Offset / server.config.BlockSize
				if nextExtent.Asset != extent.Asset || nextBlock != blockNumber {
					server.blocks.Prefetch(nextExtent.Asset, document.Assets[nextExtent.Asset], nextBlock)
				}
			}
			_, err = block.Seek(offsetInBlock, io.SeekStart)
			if err == nil {
				_, err = io.CopyN(response, block, length)
			}
			block.Close()
			if err != nil {
				log.Printf("stream %s failed: %v", request.URL.Path, err)
				return
			}
			first = false
			position += length
			remaining -= length
		}
	}
}

func writeBytes(response http.ResponseWriter, request *http.Request, contentType string, data []byte) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Content-Length", fmt.Sprint(len(data)))
	response.Header().Set("Cache-Control", "public, max-age=60")
	if request.Method == http.MethodGet {
		_, _ = response.Write(data)
	}
}

func (store *indexStore) load() error {
	if err := store.refresh(); err == nil {
		return nil
	} else {
		log.Printf("remote index unavailable: %v", err)
	}
	data, err := os.ReadFile(store.cacheFile)
	if err != nil {
		return fmt.Errorf("no remote or cached index is available: %w", err)
	}
	document, err := cacheindex.Parse(data, store.assetPrefix)
	if err != nil {
		return fmt.Errorf("parse cached index: %w", err)
	}
	if document.PublicKey != store.publicKey {
		return fmt.Errorf("cached index public key does not match configured key")
	}
	store.current = document
	return nil
}

func (store *indexStore) refresh() error {
	request, err := http.NewRequest(http.MethodGet, store.url, nil)
	if err != nil {
		return fmt.Errorf("create index request: %w", err)
	}
	request.Header.Set("User-Agent", "nix-cache-proxy/1")
	response, err := store.client.Do(request)
	if err != nil {
		return fmt.Errorf("download index: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download index: status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 128*1024*1024))
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}
	document, err := cacheindex.Parse(data, store.assetPrefix)
	if err != nil {
		return err
	}
	if document.PublicKey != store.publicKey {
		return fmt.Errorf("remote index public key does not match configured key")
	}
	if err := os.MkdirAll(filepath.Dir(store.cacheFile), 0o750); err != nil {
		return fmt.Errorf("create index cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(store.cacheFile), ".index-*")
	if err != nil {
		return fmt.Errorf("create temporary index: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write index: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close index: %w", err)
	}
	if err := os.Rename(temporaryName, store.cacheFile); err != nil {
		return fmt.Errorf("publish index: %w", err)
	}
	store.mu.Lock()
	store.current = document
	store.mu.Unlock()
	return nil
}

func (store *indexStore) get() *cacheindex.Document {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.current
}

func (server *Server) refresh(ctx context.Context) {
	ticker := time.NewTicker(server.config.RefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := server.index.refresh(); err != nil {
				log.Printf("index refresh failed; retaining current index: %v", err)
			}
		}
	}
}

func FreeListenAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}
