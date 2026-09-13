package filter

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

type Options struct {
	PathsFile     string
	PreviousIndex string
	CacheURL      string
	Output        string
	Workers       int
	Timeout       time.Duration
}

type Result struct {
	Selected        int
	AlreadyIndexed  int
	AvailableRemote int
}

type candidate struct {
	path      string
	storeHash string
}

func Run(options Options) (Result, error) {
	if options.Workers <= 0 {
		return Result{}, fmt.Errorf("workers must be positive")
	}
	paths, err := readPaths(options.PathsFile)
	if err != nil {
		return Result{}, err
	}
	indexed, err := readIndexedPaths(options.PreviousIndex)
	if err != nil {
		return Result{}, err
	}

	jobs := make(chan candidate)
	selected := make(chan string)
	available := make(chan struct{})
	client := &http.Client{Timeout: options.Timeout}
	var workers sync.WaitGroup
	for range options.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range jobs {
				request, err := http.NewRequestWithContext(context.Background(), http.MethodHead, strings.TrimRight(options.CacheURL, "/")+"/"+item.storeHash+".narinfo", nil)
				if err == nil {
					request.Header.Set("User-Agent", "nix-cache/1")
					response, requestErr := client.Do(request)
					if requestErr == nil {
						response.Body.Close()
						if response.StatusCode == http.StatusOK {
							available <- struct{}{}
							continue
						}
					}
				}
				selected <- item.path
			}
		}()
	}
	result := Result{}
	go func() {
		for _, path := range paths {
			storeHash := storeHash(path)
			if !cacheindex.ValidStoreHash(storeHash) {
				continue
			}
			if indexed[storeHash] {
				result.AlreadyIndexed++
				continue
			}
			jobs <- candidate{path: path, storeHash: storeHash}
		}
		close(jobs)
		workers.Wait()
		close(selected)
		close(available)
	}()

	var output []string
	for selected != nil || available != nil {
		select {
		case path, open := <-selected:
			if !open {
				selected = nil
				continue
			}
			output = append(output, path)
		case _, open := <-available:
			if !open {
				available = nil
				continue
			}
			result.AvailableRemote++
		}
	}
	sort.Strings(output)
	result.Selected = len(output)
	data := []byte(strings.Join(output, "\n"))
	if len(data) > 0 {
		data = append(data, '\n')
	}
	if err := os.WriteFile(options.Output, data, 0o644); err != nil {
		return Result{}, fmt.Errorf("write selected paths: %w", err)
	}
	return result, nil
}

func readPaths(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open paths file: %w", err)
	}
	defer file.Close()
	seen := make(map[string]bool)
	var paths []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		path := strings.TrimSpace(scanner.Text())
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read paths file: %w", err)
	}
	return paths, nil
}

func readIndexedPaths(path string) (map[string]bool, error) {
	indexed := make(map[string]bool)
	if path == "" {
		return indexed, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return indexed, nil
		}
		return nil, fmt.Errorf("read previous index: %w", err)
	}
	document, err := cacheindex.Parse(data, "")
	if err != nil {
		return nil, fmt.Errorf("parse previous index: %w", err)
	}
	for storeHash := range document.Paths {
		indexed[storeHash] = true
	}
	return indexed, nil
}

func storeHash(path string) string {
	base := filepath.Base(path)
	hash, _, _ := strings.Cut(base, "-")
	return hash
}
