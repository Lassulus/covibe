{
  description = "covibe — co-vibing sessions for omp: launch omp in a mux, stream each session to a built-in OIDC-gated collab relay, and serve a QR dashboard + browser viewer";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      systemOutputs = flake-utils.lib.eachDefaultSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          covibe = pkgs.callPackage ./nix/package.nix {
            version = self.shortRev or self.dirtyShortRev or "dev";
          };
        in
        {
          packages = {
            default = covibe;
            covibe = covibe;
          };

          apps.default = {
            type = "app";
            program = "${covibe}/bin/covibe";
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.go-tools
              pkgs.zellij
              pkgs.tmux
            ];
          };

          checks = {
            covibe = covibe;
          }
          // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
            integration = import ./nix/test.nix { inherit pkgs self; };
          };
        }
      );
    in
    systemOutputs
    // {
      nixosModules.default = import ./nix/module.nix self;
      nixosModules.covibe = self.nixosModules.default;

      overlays.default = final: _prev: {
        covibe = final.callPackage ./nix/package.nix { };
      };
    };
}
