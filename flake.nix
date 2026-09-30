{
  description = "GitHub Releases-backed Nix binary cache";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, self }:
    let
      eachSystem = nixpkgs.lib.genAttrs [
        "aarch64-linux"
        "x86_64-linux"
      ];
    in
    {
      nixosModules.default = import ./nix/module.nix { inherit self; };

      packages = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          nix-cache = pkgs.buildGoModule {
            pname = "nix-cache";
            version = "0.1.0";
            src = self;
            vendorHash = null;
            subPackages = [ "cmd/nix-cache" ];
          };
          default = nix-cache;
        }
      );

      checks = eachSystem (system: {
        inherit (self.packages.${system}) nix-cache;
      });
    };
}
