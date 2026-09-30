{ self }:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.nix-cache;
  substituter = "http://127.0.0.1:${toString cfg.port}";
  releaseURL = "https://github.com/${cfg.repository}/releases";
in
{
  options.services.nix-cache = {
    enable = lib.mkEnableOption "GitHub Releases-backed Nix binary cache proxy";

    repository = lib.mkOption {
      type = lib.types.str;
      default = "divyam234/nix-cache";
      example = "my-user/my-cache";
      description = "GitHub owner/repository hosting the cache releases.";
    };

    publicKey = lib.mkOption {
      type = lib.types.str;
      default = lib.removeSuffix "\n" (builtins.readFile ../public-key.txt);
      description = "Public key used to verify cache signatures.";
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 7745;
      description = "Local port used by the proxy and Nix substituter.";
    };

    refreshInterval = lib.mkOption {
      type = lib.types.str;
      default = "1h";
      example = "15m";
      description = "How often the proxy refreshes the release index (Go duration).";
    };
  };

  config = lib.mkIf cfg.enable {
    nix.settings.substituters = [ substituter ];
    nix.settings.trusted-public-keys = [ cfg.publicKey ];

    systemd.services.nix-cache-proxy = {
      description = "GitHub Releases-backed Nix binary cache proxy";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        CacheDirectory = "nix-cache-proxy";
        DynamicUser = true;
        ExecStart = lib.escapeShellArgs [
          "${self.packages.${pkgs.stdenv.hostPlatform.system}.default}/bin/nix-cache"
          "serve"
          "--listen"
          "127.0.0.1:${toString cfg.port}"
          "--index-url"
          "${releaseURL}/latest/download/index.json"
          "--index-cache"
          "/var/cache/nix-cache-proxy/index.json"
          "--asset-url-prefix"
          "${releaseURL}/download/"
          "--public-key"
          cfg.publicKey
          "--refresh-interval"
          cfg.refreshInterval
        ];
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectHome = true;
        ProtectSystem = "strict";
        Restart = "on-failure";
        RestartSec = 30;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
        ];
      };
    };
  };
}
