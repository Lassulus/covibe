# `covibe` with a backend baked in: the CLI reads COVIBE_DASHBOARD,
# COVIBE_RELAY_HOST, COVIBE_WEB_CLIENT, COVIBE_LOCAL_RELAY and COVIBE_OMP, so
# pointing a machine at a covibe deployment is purely a matter of defaults — no
# flags at the call site and no wrapper script.
#
# Every value is set with --set-default, so the environment still wins at
# runtime, and each address is an argument: covibe hardcodes no deployment.
#
# `defaultArgs` is prepended to the command line. Pass [ "session" "--" ] for a
# one-word launcher whose arguments go to omp (`covibe --resume` in a project
# directory hosts that directory); leave it empty to keep the full CLI.
{
  lib,
  stdenvNoCC,
  makeBinaryWrapper,
  covibe,
  omp,
  dashboard ? "",
  relayHost ? "",
  webClient ? "",
  localRelay ? "",
  defaultArgs ? [ ],
}:

stdenvNoCC.mkDerivation {
  pname = "covibe-client";
  inherit (covibe) version;

  dontUnpack = true;
  nativeBuildInputs = [ makeBinaryWrapper ];

  installPhase = ''
    runHook preInstall
    mkdir -p $out/bin
    makeWrapper ${lib.getExe covibe} $out/bin/covibe \
      ${lib.concatMapStringsSep " " (a: "--add-flags ${lib.escapeShellArg a}") defaultArgs} \
      --set-default COVIBE_OMP ${lib.getBin omp}/bin/omp \
      ${lib.optionalString (dashboard != "") "--set-default COVIBE_DASHBOARD ${lib.escapeShellArg dashboard}"} \
      ${lib.optionalString (relayHost != "") "--set-default COVIBE_RELAY_HOST ${lib.escapeShellArg relayHost}"} \
      ${lib.optionalString (webClient != "") "--set-default COVIBE_WEB_CLIENT ${lib.escapeShellArg webClient}"} \
      ${lib.optionalString (localRelay != "") "--set-default COVIBE_LOCAL_RELAY ${lib.escapeShellArg localRelay}"}
    runHook postInstall
  '';

  meta = covibe.meta or { } // {
    description = "covibe CLI pointed at a specific covibe deployment";
    mainProgram = "covibe";
  };
}
