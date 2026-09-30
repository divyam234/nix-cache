# nix-cache

A Nix binary cache hosted in GitHub Releases. On NixOS, enable the module to
run the proxy and configure Nix automatically. You do not need to run the
publishing workflow to use the existing cache.

## Use this cache

Add the flake as an input and import its NixOS module:

```nix
{
  inputs.nix-cache.url = "github:divyam234/nix-cache";

  outputs = { nixpkgs, nix-cache, ... }: {
    nixosConfigurations.my-host = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        nix-cache.nixosModules.default
        ({ ... }: {
          services.nix-cache.enable = true;
        })
        ./configuration.nix
      ];
    };
  };
}
```

The module starts `nix-cache-proxy` on `127.0.0.1:7745` and adds that URL and
this cache's public key to Nix's settings. To change the local port or how
often the proxy checks for a new index:

```nix
services.nix-cache.port = 7750;
services.nix-cache.refreshInterval = "15m";
services.nix-cache.repository = "my-user/my-cache";
services.nix-cache.publicKey = "my-cache-1:YOUR_PUBLIC_KEY";
```

The port is used in both the systemd service and the Nix substituter. The
refresh interval defaults to `1h` and uses Go duration syntax (for example,
`30m` or `2h`). The repository defaults to `divyam234/nix-cache`, and the public
key defaults to this flake's `public-key.txt`. Set both options when using a
different cache; release URLs are derived from the repository.

### Run it without the module

You need Nix with flakes enabled and network access to GitHub Releases. Start
the proxy in a terminal (keep it running while you build):

```sh
nix run github:divyam234/nix-cache -- serve \
  --index-url https://github.com/divyam234/nix-cache/releases/latest/download/index.json \
  --index-cache "$HOME/.cache/nix-cache/index.json" \
  --asset-url-prefix https://github.com/divyam234/nix-cache/releases/download/ \
  --public-key 'nix-cache-1:833kjCWb6yhgpaUIez65hOJBJUZDkns+ybXW/WJMsYI='
```

The proxy listens on `127.0.0.1:7745` by default. In another terminal, use it
for a build:

```sh
nix build .#your-package \
  --option extra-substituters http://127.0.0.1:7745 \
  --option extra-trusted-public-keys 'nix-cache-1:833kjCWb6yhgpaUIez65hOJBJUZDkns+ybXW/WJMsYI='
```

Replace `.#your-package` with your own installable. This cache only contains
the closures published by this repository; it is not a general-purpose mirror
of `cache.nixos.org`. An empty cache hit rate for unrelated packages is normal.
If your Nix installation restricts substituter settings to trusted users,
configure the URL and public key in your Nix daemon configuration instead.

The proxy saves the last valid index at `--index-cache` and can use it when
GitHub is temporarily unavailable. NAR data is streamed from release assets,
not cached locally. Stop the proxy with Ctrl-C.

## Publish your own cache

Forking this repository is **not** enough to publish a working cache: the
workflow checks out `divyam234/dotfiles`. To adapt it:

1. In `.github/workflows/publish.yml`, change the dotfiles checkout repository
   in both the discovery and build jobs. The workflow discovers all
   `nixosConfigurations` system builds and `homeConfigurations` activation
   packages from that flake, then groups them by derivation system. The runner
   mapping currently supports `x86_64-linux` and `aarch64-linux`; an unknown
   system fails discovery rather than being skipped. The workflow can be
   started manually in Actions; without a dispatch SHA it builds the source
   repository's `main` branch.
2. Generate your own signing key and keep the secret private:

   ```sh
   nix key generate-secret --key-name my-cache-1 > cache-secret.key
   nix key convert-secret-to-public < cache-secret.key
   ```

   Store the *entire contents* of `cache-secret.key` as the fork's Actions
   secret `CACHE_SIGNING_KEY`. Do not commit the secret key. Replace
   `CACHE_PUBLIC_KEY` in the workflow with the printed public key; use the same
   public key when starting your proxy and configuring Nix. Replace
   `public-key.txt` in your fork to change the module's default, or set
   `services.nix-cache.publicKey` explicitly. The example commands above
   contain **this repository's** key, not yours.
3. Replace the repository-specific URLs in your proxy command with your fork's
   Releases URLs. The workflow's `GITHUB_REPOSITORY`-based asset URLs already
   follow the fork. Trigger the `publish` workflow manually and confirm that
   its latest release contains `index.json` before using the fork as a cache.

If another repository should publish automatically, give its workflow a token
with permission to dispatch events to your fork and send a
`dotfiles-flake-updated` repository dispatch with `client_payload.sha` set to
the source commit to build. The sender must dispatch *after* pushing that
commit. See `.github/workflows/publish.yml` for the expected event; there is
no need to copy the example automation from this README.

## How it works

The workflow builds the configured closures, excludes paths already present
in `cache.nixos.org`, and signs the remaining raw NARs. It packs them into
chunks (up to 1 GiB each) and uploads per-architecture indexes to a draft
release. After merging those indexes into `index.json`, it publishes the
release. The proxy exposes standard Nix binary-cache endpoints and fetches
NARs via validated byte-range requests to the immutable chunk assets.

Cleanup keeps the two newest published cache generations and any older assets
referenced by their indexes; it removes older releases and tags, including
orphaned releases or tags. A cached index may outlive its release, so refresh
the proxy after generations expire if downloads start returning 404.

## Development

```sh
go test ./...
nix flake check
```
