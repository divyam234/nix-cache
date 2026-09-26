package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rangeSource struct {
	client    *http.Client
	limit     chan struct{}
	mu        sync.Mutex
	redirects map[string]redirect
}

type redirect struct {
	url     string
	expires time.Time
}

func newRangeSource(client *http.Client, maxDownloads int) (*rangeSource, error) {
	if maxDownloads <= 0 {
		return nil, fmt.Errorf("maximum downloads must be positive")
	}
	return &rangeSource{client: client, limit: make(chan struct{}, maxDownloads), redirects: make(map[string]redirect)}, nil
}

func (source *rangeSource) cachedURL(origin string) string {
	source.mu.Lock()
	defer source.mu.Unlock()
	if entry, ok := source.redirects[origin]; ok {
		if time.Now().Add(15 * time.Second).Before(entry.expires) {
			return entry.url
		}
		delete(source.redirects, origin)
	}
	return origin
}

func (source *rangeSource) remember(origin string, response *http.Response) {
	if response.Request.URL.String() == origin {
		return
	}
	parsed := response.Request.URL
	expires, err := time.Parse(time.RFC3339, parsed.Query().Get("se"))
	if err != nil || !time.Now().Before(expires) {
		return
	}
	source.mu.Lock()
	source.redirects[origin] = redirect{url: parsed.String(), expires: expires}
	source.mu.Unlock()
}

func (source *rangeSource) open(ctx context.Context, origin string, start, length, size int64) (*http.Response, error) {
	end := start + length - 1
	if start < 0 || length <= 0 || end < start || end >= size {
		return nil, fmt.Errorf("range outside asset")
	}
	for attempt := 0; attempt < 2; attempt++ {
		address := source.cachedURL(origin)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", "nix-cache-proxy/1")
		response, err := source.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("download range: %w", err)
		}
		if address != origin && (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusNotFound) {
			response.Body.Close()
			source.mu.Lock()
			delete(source.redirects, origin)
			source.mu.Unlock()
			continue
		}
		if response.StatusCode != http.StatusPartialContent {
			response.Body.Close()
			return nil, fmt.Errorf("upstream ignored range: status %d", response.StatusCode)
		}
		if err := validateContentRange(response.Header.Get("Content-Range"), start, end, size); err != nil {
			response.Body.Close()
			return nil, err
		}
		if response.ContentLength != length {
			response.Body.Close()
			return nil, fmt.Errorf("upstream Content-Length is %d, want %d", response.ContentLength, length)
		}
		source.remember(origin, response)
		return response, nil
	}
	return nil, fmt.Errorf("redirect refresh failed")
}

func (source *rangeSource) copy(ctx context.Context, writer io.Writer, origin string, start, length, size int64) (int64, error) {
	select {
	case source.limit <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-source.limit }()
	var total int64
	for total < length {
		response, err := source.open(ctx, origin, start+total, length-total, size)
		if err != nil {
			return total, err
		}
		tracked := &errorWriter{Writer: writer}
		written, copyErr := io.CopyN(tracked, response.Body, length-total)
		response.Body.Close()
		total += written
		if copyErr == nil {
			return total, nil
		}
		if tracked.err != nil {
			return total, tracked.err
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		if written == 0 {
			return total, fmt.Errorf("range stream failed: %w", copyErr)
		}
	}
	return total, nil
}

type errorWriter struct {
	io.Writer
	err error
}

func (writer *errorWriter) Write(data []byte) (int, error) {
	n, err := writer.Writer.Write(data)
	writer.err = err
	return n, err
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
