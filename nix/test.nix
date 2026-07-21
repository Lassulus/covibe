# NixOS VM test: boots the covibe module, then drives the dashboard over HTTP
# to prove the full path — service up, web-initiated session creation, mux
# launch, and /collab-link capture — works end to end.
#
# omp is not packaged, so a fake `omp` stands in: it emits a realistic /collab
# banner when the session wrapper auto-types /collab, which is exactly what the
# link sniffer consumes. tmux is the mux (headless, unlike zellij). Auth runs
# in no-auth mode so the test needs no OIDC provider (the OIDC path is covered
# by unit tests).
{ pkgs, self }:
let
  fakeOmp = pkgs.writeShellScriptBin "omp" ''
    room='abcdefgh12.keykeykeykeykeykeykeyZZ'
    printf 'fake omp ready\r\n'
    while IFS= read -r line; do
      case "$line" in
        */collab*)
          printf 'Collab session started!\r\n'
          printf ' Join from another terminal: omp join "%s"\r\n' "$room"
          ;;
        */quit*) exit 0 ;;
      esac
    done
  '';
in
pkgs.testers.runNixOSTest {
  name = "covibe";

  nodes.machine =
    { ... }:
    {
      imports = [ self.nixosModules.default ];

      users.users.vibe = {
        isNormalUser = true;
        home = "/home/vibe";
      };

      services.covibe = {
        enable = true;
        user = "vibe";
        group = "users";
        mux = "tmux";
        relay = "wss://relay.test:7475";
        ompPackage = fakeOmp;
        dashboard = {
          addr = "127.0.0.1:8770";
          noAuth = true;
          insecure = true;
          workspaceRoot = "/home/vibe/covibe";
        };
      };

      environment.systemPackages = [ pkgs.curl ];
      virtualisation.graphics = false;
    };

  testScript = ''
    start_all()
    machine.wait_for_unit("covibe-dashboard.service")
    machine.wait_for_open_port(8770)

    # Liveness.
    machine.succeed("curl -sf http://127.0.0.1:8770/healthz")

    # Empty to start with.
    machine.succeed("curl -sf http://127.0.0.1:8770/api/sessions | grep -q '^\[\]'")

    # Path-traversal names are rejected.
    code = machine.succeed(
        "curl -s -o /dev/null -w '%{http_code}' -X POST "
        "http://127.0.0.1:8770/api/sessions "
        "-H 'Content-Type: application/json' -d '{\"name\":\"../evil\"}'"
    )
    assert code == "400", f"traversal name should be 400, got {code}"

    # Create a session from the web.
    machine.succeed(
        "curl -sf -X POST http://127.0.0.1:8770/api/sessions "
        "-H 'Content-Type: application/json' -d '{\"name\":\"vmproj\"}'"
    )

    # The workspace dir is created, clamped inside the workspace root.
    machine.succeed("test -d /home/vibe/covibe/vmproj")

    # The wrapper auto-runs /collab; wait for the captured link to surface.
    machine.wait_until_succeeds(
        "curl -sf http://127.0.0.1:8770/api/sessions | "
        "grep -q '\"joinLink\":\"abcdefgh12.keykeykeykeykeykeykeyZZ\"'",
        timeout=60,
    )

    sessions = machine.succeed("curl -sf http://127.0.0.1:8770/api/sessions")
    assert '"name":"vmproj"' in sessions, sessions
    assert '"status":"live"' in sessions, sessions
    assert '"browserUrl":"https://relay.test:7475/#abcdefgh12' in sessions, sessions
    assert '"qr":"data:image/png;base64,' in sessions, sessions

    # The session runs in the shared tmux server, attachable by the user.
    machine.succeed("su vibe -c 'TMUX_TMPDIR=/run/covibe tmux ls' | grep -q '^covibe:'")

    # The covibe CLI sees the same live session from the shared spool.
    machine.succeed(
        "su vibe -c 'COVIBE_STATE_DIR=/run/covibe covibe list' | grep -q vmproj"
    )

    # Killing the tmux window ends the session; the dashboard prunes it.
    machine.succeed("su vibe -c 'TMUX_TMPDIR=/run/covibe tmux kill-session -t covibe'")
    machine.wait_until_succeeds(
        "curl -sf http://127.0.0.1:8770/api/sessions | grep -q '^\[\]'",
        timeout=30,
    )
  '';
}
