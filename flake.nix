{
  description = "covibe — co-vibing sessions for omp: launch omp in tmux, stream each session to a built-in OIDC-gated collab relay, and serve a QR dashboard + browser viewer";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    # Source of the unpatched omp. A consumer that already has llm-agents should
    # set inputs.covibe.inputs.llm-agents.follows to share one omp revision (and
    # one build) instead of pinning a second.
    llm-agents.url = "github:numtide/llm-agents.nix";
    llm-agents.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      llm-agents,
    }:
    let
      systemOutputs = flake-utils.lib.eachDefaultSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          covibe = pkgs.callPackage ./nix/package.nix {
            version = self.shortRev or self.dirtyShortRev or "dev";
          };
          omp = pkgs.callPackage ./nix/omp.nix {
            baseOmp = llm-agents.packages.${system}.omp;
          };
          # Rust sidecar holding a session's iroh endpoints, so the peer-to-peer
          # terminal uses iroh first-party instead of reaching it through a Go port.
          covibe-p2p = pkgs.callPackage ./nix/p2p.nix { };
        in
        {
          packages = {
            default = covibe;
            covibe = covibe;
            # omp carrying covibe's collab patch + the self-hosted collab-web SPA.
            omp = omp;
            covibe-p2p = covibe-p2p;
            # covibe CLI with no backend baked in; override the addresses:
            #   covibe-client.override { dashboard = "https://covibe.example"; }
            covibe-client = pkgs.callPackage ./nix/client.nix {
              inherit covibe omp covibe-p2p;
            };
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
              pkgs.tmux
              pkgs.cargo
              pkgs.rustc
              pkgs.rustfmt
              pkgs.clippy
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
