# omp patched for covibe-owned collab.
#
# Two additions on top of the upstream package:
#
#   1. Supervised, env-driven headless collab hosting (OMP_COLLAB_RELAY/_ROOM/
#      _KEY). covibe mints the room up front and hands it over, so it owns a
#      stable, enumerable link with no minting or output scraping, and hosting
#      survives relay drops and session switches (see the patch header).
#   2. A self-hosted collab-web SPA at share/collab-web, built with --public-path
#      /c/ and with the external analytics script stripped, which the dashboard
#      serves at /c/. Nothing loads from my.omp.sh.
#
# `baseOmp` is the unpatched package (llm-agents' omp); it stays an argument so a
# consumer can pin its own revision.
{ baseOmp }:

baseOmp.overrideAttrs (o: {
  patches = (o.patches or [ ]) ++ [ ./omp-collab-autostart.patch ];

  postBuild = (o.postBuild or "") + ''
    echo "Building self-hosted collab-web (base /c/)..."
    sed -i '/um\.can\.ac/d' packages/collab-web/index.html
    (
      cd packages/collab-web
      bun build ./index.html --outdir dist --minify \
        --entry-naming '[hash].[ext]' --chunk-naming '[hash].[ext]' --asset-naming '[hash].[ext]' \
        --public-path /c/
      mv dist/*.html dist/index.html
      cp -R public/. dist/
    )
  '';

  postInstall = (o.postInstall or "") + ''
    mkdir -p $out/share
    cp -R packages/collab-web/dist $out/share/collab-web
  '';
})
