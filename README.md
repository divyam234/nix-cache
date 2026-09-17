# nix-cache

This repository stores a signed Nix binary cache in immutable GitHub Release
assets. Each release is a complete cache generation, and the latest two
generations are retained. Raw, uncompressed NARs are concatenated into 1 GiB
pack assets. A small local proxy translates the standard Nix binary-cache
protocol into validated GitHub byte-range requests.

## Layout

Each published release contains:

```text
chunk-x86-64-1-0000.bin
chunk-aarch64-1-0000.bin
index.json
```

`index.json` contains signed narinfo text and maps each NAR to one or more
`asset`, `offset`, and `length` extents. Releases and chunks are immutable. The
latest complete release is exposed through GitHub's `releases/latest` URL, and
the previous release remains available for clients refreshing an older index.

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

After `divyam234/dotfiles` updates its flake inputs, it dispatches the
`dotfiles-flake-updated` repository event with the new commit SHA. The publish
workflow builds the laptop, homelab, netcup, and standalone Home Manager
closures from that exact commit. It selects paths absent from
`cache.nixos.org`, signs them, streams `nix nar pack` directly into raw chunks,
and publishes the draft release only after both architecture shards have been
merged into a valid, self-contained index. After publication, releases older
than the latest two cache generations are deleted. The workflow can also be
dispatched manually, in which case it builds the current `dotfiles` main
branch.

The dotfiles workflow needs a fine-grained token with write access to this
repository stored as `NIX_CACHE_DISPATCH_TOKEN`. Its commit step should expose
whether it created a commit and that commit's SHA, then dispatch only after a
successful update:

```yaml
- name: Commit and push lock file
  id: commit
  run: |
    if git diff --quiet -- flake.lock; then
      echo "updated=false" >> "$GITHUB_OUTPUT"
      exit 0
    fi

    git config user.name "github-actions[bot]"
    git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
    git add flake.lock
    git commit -m "chore: update flake inputs"
    git push origin HEAD:main
    echo "updated=true" >> "$GITHUB_OUTPUT"
    echo "sha=$(git rev-parse HEAD)" >> "$GITHUB_OUTPUT"

- name: Publish Nix cache
  if: steps.commit.outputs.updated == 'true'
  env:
    GH_TOKEN: ${{ secrets.NIX_CACHE_DISPATCH_TOKEN }}
    SOURCE_SHA: ${{ steps.commit.outputs.sha }}
  run: |
    gh api --method POST repos/divyam234/nix-cache/dispatches \
      -f event_type=dotfiles-flake-updated \
      -f "client_payload[sha]=$SOURCE_SHA"
```

Run tests with:

```console
go test ./...
nix flake check
```
