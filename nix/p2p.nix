{
  lib,
  rustPlatform,
  pkg-config,
}:

rustPlatform.buildRustPackage {
  pname = "covibe-p2p";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../p2p;
    fileset = lib.fileset.unions [
      ../p2p/Cargo.toml
      ../p2p/Cargo.lock
      ../p2p/src
    ];
  };

  cargoLock.lockFile = ../p2p/Cargo.lock;

  nativeBuildInputs = [ pkg-config ];

  meta = {
    description = "Dumb byte-pump sidecar bridging unix sockets to iroh QUIC streams for covibe";
    mainProgram = "covibe-p2p";
    license = lib.licenses.mit;
    platforms = lib.platforms.unix;
  };
}
