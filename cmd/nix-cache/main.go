package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	cachefilter "github.com/divyam234/nix-cache/internal/filter"
	"github.com/divyam234/nix-cache/internal/pack"
	"github.com/divyam234/nix-cache/internal/proxy"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "pack":
		err = runPack(os.Args[2:])
	case "merge":
		err = runMerge(os.Args[2:])
	case "filter-upstream":
		err = runFilter(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: nix-cache <filter-upstream|pack|merge|serve> [options]")
	os.Exit(2)
}

func runFilter(arguments []string) error {
	flags := flag.NewFlagSet("filter-upstream", flag.ContinueOnError)
	options := cachefilter.Options{}
	flags.StringVar(&options.PathsFile, "paths-file", "", "candidate store paths")
	flags.StringVar(&options.PreviousIndex, "previous-index", "", "optional previous index")
	flags.StringVar(&options.CacheURL, "cache-url", "https://cache.nixos.org", "upstream binary cache")
	flags.StringVar(&options.Output, "output", "", "selected store paths")
	flags.IntVar(&options.Workers, "workers", 32, "concurrent metadata requests")
	flags.DurationVar(&options.Timeout, "timeout", 30*time.Second, "request timeout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if options.PathsFile == "" || options.Output == "" {
		return fmt.Errorf("paths-file and output are required")
	}
	result, err := cachefilter.Run(options)
	if err == nil {
		log.Printf(
			"selected %d paths; skipped %d already indexed and %d available from %s",
			result.Selected,
			result.AlreadyIndexed,
			result.AvailableRemote,
			options.CacheURL,
		)
	}
	return err
}

func runPack(arguments []string) error {
	flags := flag.NewFlagSet("pack", flag.ContinueOnError)
	var options pack.Options
	flags.StringVar(&options.CacheDir, "cache-dir", "", "source file binary cache")
	flags.StringVar(&options.OutputDir, "output", "", "output directory")
	flags.StringVar(&options.AssetBaseURL, "asset-base-url", "", "immutable release asset URL prefix")
	flags.StringVar(&options.Prefix, "prefix", "", "unique chunk name prefix")
	flags.StringVar(&options.PublicKey, "public-key", "", "binary cache public key")
	flags.StringVar(&options.PreviousIndex, "previous-index", "", "optional previous index")
	flags.StringVar(&options.DropFile, "drop-file", "", "store paths or hashes to exclude")
	flags.StringVar(&options.IndexName, "index-name", "index.json", "shard index filename")
	flags.Int64Var(&options.ChunkSize, "chunk-size", 1024*1024*1024, "release chunk size in bytes")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if options.CacheDir == "" || options.OutputDir == "" || options.AssetBaseURL == "" || options.Prefix == "" || options.PublicKey == "" {
		return fmt.Errorf("cache-dir, output, asset-base-url, prefix, and public-key are required")
	}
	added, skipped, err := pack.Run(options)
	if err == nil {
		log.Printf("packed %d paths; skipped %d", added, skipped)
	}
	return err
}

func runMerge(arguments []string) error {
	flags := flag.NewFlagSet("merge", flag.ContinueOnError)
	publicKey := flags.String("public-key", "", "binary cache public key")
	output := flags.String("output", "", "output index")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *publicKey == "" || *output == "" || flags.NArg() == 0 {
		return fmt.Errorf("public-key, output, and at least one index are required")
	}
	return pack.Merge(*publicKey, *output, flags.Args())
}

func runServe(arguments []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	config := proxy.Config{}
	flags.StringVar(&config.Listen, "listen", "127.0.0.1:7745", "HTTP listen address")
	flags.StringVar(&config.IndexURL, "index-url", "", "latest index URL")
	flags.StringVar(&config.IndexCache, "index-cache", "", "persistent index path")
	flags.StringVar(&config.AssetURLPrefix, "asset-url-prefix", "", "allowed release asset URL prefix")
	flags.StringVar(&config.PublicKey, "public-key", "", "expected binary cache public key")
	flags.StringVar(&config.BlockDirectory, "block-cache", "", "persistent block cache directory")
	flags.Int64Var(&config.BlockSize, "block-size", 32*1024*1024, "download block size in bytes")
	flags.Int64Var(&config.MaxCacheSize, "max-cache-size", 20*1024*1024*1024, "maximum persistent block bytes; zero is unlimited")
	flags.IntVar(&config.MaxDownloads, "max-downloads", 8, "maximum concurrent GitHub range requests")
	flags.DurationVar(&config.RefreshEvery, "refresh-interval", time.Hour, "index refresh interval")
	flags.DurationVar(&config.HTTPTimeout, "http-timeout", 5*time.Minute, "upstream request timeout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if config.IndexURL == "" || config.IndexCache == "" || config.AssetURLPrefix == "" || config.PublicKey == "" || config.BlockDirectory == "" {
		return fmt.Errorf("index-url, index-cache, asset-url-prefix, public-key, and block-cache are required")
	}
	server, err := proxy.New(config)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return server.ListenAndServe(ctx)
}
