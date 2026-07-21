{
  lib,
  buildGoModule,
  version ? "dev",
}:

buildGoModule {
  pname = "covibe";
  inherit version;

  src =
    let
      fs = lib.fileset;
    in
    fs.toSource {
      root = ../.;
      fileset = fs.unions [
        ../go.mod
        ../go.sum
        ../main.go
        ../vendor
        ../internal
      ];
    };

  # Dependencies are vendored in-tree; no network fetch during build.
  vendorHash = null;

  # Pure-Go build: no cgo resolver, static-ish binary, trivial to package.
  # NOTE: govulncheck flags GO-2026-5856 (crypto/tls ECH privacy leak), fixed in
  # go1.26.5. Low impact (ECH unused); clears automatically once nixpkgs ships
  # Go >= 1.26.5 (currently 1.26.4).
  env.CGO_ENABLED = "0";

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  meta = {
    description = "Co-vibing sessions for omp: mux launcher + OIDC QR dashboard";
    mainProgram = "covibe";
    license = lib.licenses.mit;
    platforms = lib.platforms.unix;
  };
}
