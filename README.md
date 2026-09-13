# nix-cache

This repository stores a signed Nix binary cache in immutable GitHub Release
assets. Raw, uncompressed NARs are concatenated into 1 GiB pack assets. A small
local proxy translates the standard Nix binary-cache protocol into validated
GitHub byte-range requests.

## Layout

Each published release contains:

```text
chunk-x86-64-1-0000.bin
chunk-aarch64-1-0000.bin
index.json
```

`index.json` contains signed narinfo text and maps each NAR to one or more
`asset`, `offset`, and `length` extents. Releases and chunks are immutable. The
latest complete release is exposed through GitHub's `releases/latest` URL.

The proxy downloads 32 MiB blocks with `Range`, requires a correct `206
Partial Content` response, coalesces concurrent requests for the same block,
resumes partial blocks, and keeps an LRU disk cache. Nix receives ordinary raw
NAR streams with `Compression: none` and verifies them with:

```text
nix-cache-1:833kjCWb6yhgpaUIez65hOJBJUZDkns+ybXW/WJMsYI=
```

## Proxy

```console
nix run github:divyam234/nix-cache -- serve \
  --index-url https://github.com/divyam234/nix-cache/releases/latest/download/index.json \
  --index-cache ./state/index.json \
  --asset-url-prefix https://github.com/divyam234/nix-cache/releases/download/ \
  --public-key 'nix-cache-1:833kjCWb6yhgpaUIez65hOJBJUZDkns+ybXW/WJMsYI=' \
  --block-cache ./state/blocks
```

Configure `http://127.0.0.1:7745` as a substituter and trust the public key
above. The proxy retains its last valid index if GitHub is temporarily
unavailable.

## Publishing

The weekly and manually dispatched workflow builds the laptop, homelab,
netcup, and standalone Home Manager closures from `divyam234/dotfiles`. It
selects paths absent from both the previous index and `cache.nixos.org`, signs
them, streams `nix nar pack` directly into raw chunks, and publishes the
draft release only after both architecture shards have been merged into a
valid index.

Run tests with:

```console
go test ./...
nix flake check
```
