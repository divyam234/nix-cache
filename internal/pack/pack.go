package pack

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/divyam234/nix-cache/internal/cacheindex"
)

type Options struct {
	CacheDir      string
	OutputDir     string
	AssetBaseURL  string
	Prefix        string
	PublicKey     string
	PreviousIndex string
	DropFile      string
	IndexName     string
	ChunkSize     int64
}

type ExportOptions struct {
	PathsFile     string
	OutputDir     string
	AssetBaseURL  string
	Prefix        string
	PublicKey     string
	SigningKey    string
	PreviousIndex string
	IndexName     string
	ChunkSize     int64
}

type pathInfo struct {
	Deriver    *string  `json:"deriver"`
	NarHash    string   `json:"narHash"`
	NarSize    int64    `json:"narSize"`
	References []string `json:"references"`
	Signatures []string `json:"signatures"`
}

type chunkWriter struct {
	outputDir    string
	prefix       string
	assetBaseURL string
	chunkSize    int64
	assets       map[string]cacheindex.Asset
	number       int
	file         *os.File
	name         string
	size         int64
}

func Run(options Options) (int, int, error) {
	if options.ChunkSize <= 0 {
		return 0, 0, errors.New("chunk size must be positive")
	}
	if options.IndexName == "" {
		options.IndexName = "index.json"
	}
	if err := os.MkdirAll(options.OutputDir, 0o755); err != nil {
		return 0, 0, fmt.Errorf("create output directory: %w", err)
	}
	document, err := loadIndex(options.PreviousIndex, options.PublicKey)
	if err != nil {
		return 0, 0, err
	}
	document.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	dropped, err := readDropFile(options.DropFile)
	if err != nil {
		return 0, 0, err
	}

	files, err := filepath.Glob(filepath.Join(options.CacheDir, "*.narinfo"))
	if err != nil {
		return 0, 0, fmt.Errorf("list narinfos: %w", err)
	}
	sort.Strings(files)
	writer := &chunkWriter{
		outputDir:    options.OutputDir,
		prefix:       options.Prefix,
		assetBaseURL: strings.TrimRight(options.AssetBaseURL, "/"),
		chunkSize:    options.ChunkSize,
		assets:       document.Assets,
	}
	added, skipped := 0, 0
	for _, narInfoPath := range files {
		storeHash := strings.TrimSuffix(filepath.Base(narInfoPath), ".narinfo")
		if !cacheindex.ValidStoreHash(storeHash) {
			return added, skipped, fmt.Errorf("invalid narinfo filename %q", filepath.Base(narInfoPath))
		}
		if _, exists := document.Paths[storeHash]; exists || dropped[storeHash] {
			skipped++
			continue
		}
		textBytes, err := os.ReadFile(narInfoPath)
		if err != nil {
			return added, skipped, fmt.Errorf("read %s: %w", narInfoPath, err)
		}
		text := string(textBytes)
		fields := parseNarInfo(text)
		if fields["Compression"] != "none" {
			return added, skipped, fmt.Errorf("%s: Compression must be none", filepath.Base(narInfoPath))
		}
		relativeURL := fields["URL"]
		if relativeURL == "" || filepath.IsAbs(relativeURL) || strings.Contains(relativeURL, "..") {
			return added, skipped, fmt.Errorf("%s: invalid NAR URL", filepath.Base(narInfoPath))
		}
		narPath := filepath.Join(options.CacheDir, filepath.FromSlash(relativeURL))
		info, err := os.Stat(narPath)
		if err != nil {
			return added, skipped, fmt.Errorf("stat NAR for %s: %w", storeHash, err)
		}
		fileSize, err := strconv.ParseInt(fields["FileSize"], 10, 64)
		if err != nil || fileSize != info.Size() {
			return added, skipped, fmt.Errorf("%s: FileSize does not match raw NAR", filepath.Base(narInfoPath))
		}
		narSize, err := strconv.ParseInt(fields["NarSize"], 10, 64)
		if err != nil || narSize != info.Size() {
			return added, skipped, fmt.Errorf("%s: NarSize does not match raw NAR", filepath.Base(narInfoPath))
		}
		extents, err := writer.add(narPath, info.Size())
		if err != nil {
			return added, skipped, err
		}
		rewritten, err := rewriteURL(text, storeHash)
		if err != nil {
			return added, skipped, fmt.Errorf("%s: %w", filepath.Base(narInfoPath), err)
		}
		document.Paths[storeHash] = cacheindex.PathEntry{
			NarInfo: rewritten,
			NAR: cacheindex.NAR{
				Size:     info.Size(),
				FileHash: fields["FileHash"],
				Extents:  extents,
			},
		}
		added++
	}
	if err := writer.close(); err != nil {
		return added, skipped, err
	}
	if err := document.Validate(""); err != nil {
		return added, skipped, fmt.Errorf("validate generated index: %w", err)
	}
	data, err := document.Marshal()
	if err != nil {
		return added, skipped, err
	}
	if err := writeAtomic(filepath.Join(options.OutputDir, options.IndexName), data); err != nil {
		return added, skipped, err
	}
	return added, skipped, nil
}

func Export(options ExportOptions) (int, error) {
	if options.ChunkSize <= 0 {
		return 0, errors.New("chunk size must be positive")
	}
	if options.IndexName == "" {
		options.IndexName = "index.json"
	}
	paths, err := readPathLines(options.PathsFile)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(options.OutputDir, 0o755); err != nil {
		return 0, fmt.Errorf("create output directory: %w", err)
	}
	document, err := loadIndex(options.PreviousIndex, options.PublicKey)
	if err != nil {
		return 0, err
	}
	document.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	if len(paths) == 0 {
		return 0, writeDocument(filepath.Join(options.OutputDir, options.IndexName), document)
	}
	if err := signPaths(paths, options.SigningKey); err != nil {
		return 0, err
	}
	metadata, err := queryPathInfo(paths)
	if err != nil {
		return 0, err
	}
	writer := &chunkWriter{
		outputDir:    options.OutputDir,
		prefix:       options.Prefix,
		assetBaseURL: strings.TrimRight(options.AssetBaseURL, "/"),
		chunkSize:    options.ChunkSize,
		assets:       document.Assets,
	}
	added := 0
	for _, path := range paths {
		storeHash := storeHashFromPath(path)
		if !cacheindex.ValidStoreHash(storeHash) {
			return added, fmt.Errorf("invalid store path %q", path)
		}
		if _, exists := document.Paths[storeHash]; exists {
			continue
		}
		info, exists := metadata[path]
		if !exists {
			return added, fmt.Errorf("nix path-info returned no metadata for %s", path)
		}
		if info.NarSize <= 0 || info.NarHash == "" {
			return added, fmt.Errorf("nix path-info returned invalid NAR metadata for %s", path)
		}
		if !hasSignature(info.Signatures, options.PublicKey) {
			return added, fmt.Errorf("%s is not signed by the configured cache key", path)
		}
		command := exec.Command("nix", "nar", "pack", path)
		stdout, err := command.StdoutPipe()
		if err != nil {
			return added, fmt.Errorf("open NAR stream for %s: %w", path, err)
		}
		var standardError bytes.Buffer
		command.Stderr = &standardError
		if err := command.Start(); err != nil {
			return added, fmt.Errorf("start NAR export for %s: %w", path, err)
		}
		extents, copyErr := writer.addReader(stdout, info.NarSize)
		extra, extraErr := io.Copy(io.Discard, stdout)
		waitErr := command.Wait()
		if copyErr != nil {
			return added, fmt.Errorf("export NAR for %s: %w", path, copyErr)
		}
		if extraErr != nil || extra != 0 {
			return added, fmt.Errorf("NAR size mismatch for %s", path)
		}
		if waitErr != nil {
			return added, fmt.Errorf("export NAR for %s: %w: %s", path, waitErr, strings.TrimSpace(standardError.String()))
		}
		document.Paths[storeHash] = cacheindex.PathEntry{
			NarInfo: makeNarInfo(path, storeHash, info),
			NAR: cacheindex.NAR{
				Size:     info.NarSize,
				FileHash: info.NarHash,
				Extents:  extents,
			},
		}
		added++
	}
	if err := writer.close(); err != nil {
		return added, err
	}
	if err := document.Validate(""); err != nil {
		return added, fmt.Errorf("validate generated index: %w", err)
	}
	return added, writeDocument(filepath.Join(options.OutputDir, options.IndexName), document)
}

func readPathLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open paths file: %w", err)
	}
	defer file.Close()
	var paths []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		if value != "" {
			paths = append(paths, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read paths file: %w", err)
	}
	sort.Strings(paths)
	return paths, nil
}

func signPaths(paths []string, signingKey string) error {
	command := exec.Command("nix", "store", "sign", "--stdin", "--key-file", signingKey)
	command.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sign store paths: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func queryPathInfo(paths []string) (map[string]pathInfo, error) {
	command := exec.Command("nix", "path-info", "--json", "--json-format", "1", "--sigs", "--stdin")
	command.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	output, err := command.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("query path metadata: %w: %s", err, strings.TrimSpace(string(exitError.Stderr)))
		}
		return nil, fmt.Errorf("query path metadata: %w", err)
	}
	var metadata map[string]pathInfo
	if err := json.Unmarshal(output, &metadata); err != nil {
		return nil, fmt.Errorf("decode path metadata: %w", err)
	}
	return metadata, nil
}

func hasSignature(signatures []string, publicKey string) bool {
	keyName, _, _ := strings.Cut(publicKey, ":")
	for _, signature := range signatures {
		if strings.HasPrefix(signature, keyName+":") {
			return true
		}
	}
	return false
}

func makeNarInfo(path, storeHash string, info pathInfo) string {
	references := make([]string, 0, len(info.References))
	for _, reference := range info.References {
		references = append(references, filepath.Base(reference))
	}
	lines := []string{
		"StorePath: " + path,
		"URL: nar/" + storeHash + ".nar",
		"Compression: none",
		"FileHash: " + info.NarHash,
		fmt.Sprintf("FileSize: %d", info.NarSize),
		"NarHash: " + info.NarHash,
		fmt.Sprintf("NarSize: %d", info.NarSize),
		"References: " + strings.Join(references, " "),
	}
	if info.Deriver != nil {
		lines = append(lines, "Deriver: "+filepath.Base(*info.Deriver))
	}
	for _, signature := range info.Signatures {
		lines = append(lines, "Sig: "+signature)
	}
	return strings.Join(lines, "\n") + "\n"
}

func storeHashFromPath(path string) string {
	base := filepath.Base(path)
	storeHash, _, _ := strings.Cut(base, "-")
	return storeHash
}

func writeDocument(path string, document *cacheindex.Document) error {
	if err := document.Validate(""); err != nil {
		return fmt.Errorf("validate generated index: %w", err)
	}
	data, err := document.Marshal()
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

func Merge(publicKey, output string, inputs []string) error {
	merged := cacheindex.Empty(publicKey, time.Now().UTC().Format(time.RFC3339))
	for _, input := range inputs {
		data, err := os.ReadFile(input)
		if err != nil {
			return fmt.Errorf("read %s: %w", input, err)
		}
		document, err := cacheindex.Parse(data, "")
		if err != nil {
			return fmt.Errorf("parse %s: %w", input, err)
		}
		if document.PublicKey != publicKey {
			return fmt.Errorf("%s: public key mismatch", input)
		}
		for name, asset := range document.Assets {
			if existing, exists := merged.Assets[name]; exists && existing != asset {
				return fmt.Errorf("conflicting asset %q", name)
			}
			merged.Assets[name] = asset
		}
		for storeHash, path := range document.Paths {
			if _, exists := merged.Paths[storeHash]; !exists {
				merged.Paths[storeHash] = path
			}
		}
	}
	if err := merged.Validate(""); err != nil {
		return fmt.Errorf("validate merged index: %w", err)
	}
	data, err := merged.Marshal()
	if err != nil {
		return err
	}
	return writeAtomic(output, data)
}

func loadIndex(path, publicKey string) (*cacheindex.Document, error) {
	if path == "" {
		return cacheindex.Empty(publicKey, time.Now().UTC().Format(time.RFC3339)), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cacheindex.Empty(publicKey, time.Now().UTC().Format(time.RFC3339)), nil
		}
		return nil, fmt.Errorf("read previous index: %w", err)
	}
	document, err := cacheindex.Parse(data, "")
	if err != nil {
		return nil, fmt.Errorf("parse previous index: %w", err)
	}
	if document.PublicKey != publicKey {
		return nil, errors.New("previous index public key does not match")
	}
	return document, nil
}

func parseNarInfo(text string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		if name, value, found := strings.Cut(line, ": "); found {
			fields[name] = value
		}
	}
	return fields
}

func rewriteURL(text, storeHash string) (string, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "URL: ") {
			lines[index] = "URL: nar/" + storeHash + ".nar"
			return strings.Join(lines, "\n") + "\n", nil
		}
	}
	return "", errors.New("narinfo has no URL field")
}

func readDropFile(path string) (map[string]bool, error) {
	dropped := make(map[string]bool)
	if path == "" {
		return dropped, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open drop file: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		base := filepath.Base(value)
		storeHash, _, _ := strings.Cut(base, "-")
		if cacheindex.ValidStoreHash(storeHash) {
			dropped[storeHash] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read drop file: %w", err)
	}
	return dropped, nil
}

func (writer *chunkWriter) open() error {
	writer.name = fmt.Sprintf("chunk-%s-%04d.bin", writer.prefix, writer.number)
	writer.number++
	file, err := os.OpenFile(filepath.Join(writer.outputDir, writer.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create chunk %s: %w", writer.name, err)
	}
	writer.file = file
	writer.size = 0
	return nil
}

func (writer *chunkWriter) close() error {
	if writer.file == nil {
		return nil
	}
	if err := writer.file.Sync(); err != nil {
		return fmt.Errorf("sync chunk %s: %w", writer.name, err)
	}
	if err := writer.file.Close(); err != nil {
		return fmt.Errorf("close chunk %s: %w", writer.name, err)
	}
	writer.assets[writer.name] = cacheindex.Asset{
		URL:  writer.assetBaseURL + "/" + writer.name,
		Size: writer.size,
	}
	writer.file = nil
	return nil
}

func (writer *chunkWriter) add(path string, size int64) ([]cacheindex.Extent, error) {
	if writer.file != nil && writer.size > 0 && size <= writer.chunkSize && writer.size+size > writer.chunkSize {
		if err := writer.close(); err != nil {
			return nil, err
		}
	}
	input, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open NAR %s: %w", path, err)
	}
	defer input.Close()
	return writer.addReader(input, size)
}

func (writer *chunkWriter) addReader(input io.Reader, size int64) ([]cacheindex.Extent, error) {
	remaining := size
	var extents []cacheindex.Extent
	for remaining > 0 {
		if writer.file == nil {
			if err := writer.open(); err != nil {
				return nil, err
			}
		}
		length := min(remaining, writer.chunkSize-writer.size)
		offset := writer.size
		written, err := io.CopyN(writer.file, input, length)
		if err != nil {
			return nil, fmt.Errorf("copy NAR stream: %w", err)
		}
		writer.size += written
		remaining -= written
		extents = append(extents, cacheindex.Extent{Asset: writer.name, Offset: offset, Length: written})
		if writer.size == writer.chunkSize {
			if err := writer.close(); err != nil {
				return nil, err
			}
		}
	}
	return extents, nil
}

func writeAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".index-*")
	if err != nil {
		return fmt.Errorf("create temporary index: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary index: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary index: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish index: %w", err)
	}
	return nil
}
