# NixOS VM test: boots the covibe module, then drives the dashboard over HTTP
# to prove the full path — service up, web-initiated session creation, mux
# launch, and /collab-link capture — works end to end.
#
# It exercises the default multiplexer (zellij): the dashboard creates a
# backgrounded zellij session (`attach --create-background`) and opens the omp
# pane in it (`zellij run`), with no client attached. omp is not packaged, so a
# fake `omp` stands in: it emits a realistic /collab banner when the session
# wrapper auto-types /collab, which is exactly what the link sniffer consumes.
# Auth runs in no-auth mode so the test needs no OIDC provider (the OIDC path is
# covered by unit tests).
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
  sock = "/run/covibe/zellij";
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
        # mux defaults to zellij — left unset on purpose to test the default.
        relay = "wss://relay.test:7475";
        ompPackage = fakeOmp;
        dashboard = {
          addr = "127.0.0.1:8770";
          noAuth = true;
          insecure = true;
          workspaceRoot = "/home/vibe/covibe";
          # Exercise the machine-facing REST surface with a real API key.
          extraSettings.COVIBE_API_KEYS = "test-key";
        };
      };

      environment.systemPackages = [ pkgs.curl ];
      virtualisation.graphics = false;
    };

  testScript = ''
    import json

    KEY = "test-key"
    API = "http://127.0.0.1:8770/api/v1/sessions"

    def zellij(cmd):
        return f"su vibe -c 'ZELLIJ_SOCKET_DIR=${sock} zellij {cmd}'"

    def api(args):
        return f"curl -sf -H 'Authorization: Bearer {KEY}' {args}"

    start_all()
    machine.wait_for_unit("covibe-dashboard.service")
    machine.wait_for_open_port(8770)

    # Liveness (public, no key).
    machine.succeed("curl -sf http://127.0.0.1:8770/healthz")

    # (API-key gating — 401 without/with a wrong key — is covered by the unit
    # test TestV1AuthGating; here noAuth is on so the browser flow works too.)
    # With a valid key the list is reachable and starts empty.
    machine.succeed(api(API) + " | grep -q '^\[\]'")

    # Path-traversal names are rejected.
    code = machine.succeed(
        "curl -s -o /dev/null -w '%{http_code}' "
        f"-H 'Authorization: Bearer {KEY}' -X POST {API} "
        "-H 'Content-Type: application/json' -d '{\"name\":\"../evil\"}'"
    )
    assert code == "400", f"traversal name should be 400, got {code}"

    # Create a session over the REST API; the response carries its id.
    created = json.loads(machine.succeed(
        api(f"-X POST {API} -H 'Content-Type: application/json' -d '{{\"name\":\"vmproj\"}}'")
    ))
    sid = created["id"]
    assert sid, created
    assert created["dir"] == "/home/vibe/covibe/vmproj", created

    # The workspace dir was created, clamped inside the workspace root.
    machine.succeed("test -d /home/vibe/covibe/vmproj")

    # A backgrounded zellij session hosts the pane.
    machine.wait_until_succeeds(zellij("list-sessions -ns") + " | grep -q '^covibe$'", timeout=30)

    # GET /sessions/<id> exposes the omp remote key once /collab is captured.
    machine.wait_until_succeeds(
        api(f"{API}/{sid}") + " | grep -q '\"joinLink\":\"abcdefgh12.keykeykeykeykeykeykeyZZ\"'",
        timeout=60,
    )
    one = json.loads(machine.succeed(api(f"{API}/{sid}")))
    assert one["name"] == "vmproj", one
    assert one["status"] == "live", one
    assert one["mux"] == "zellij", one
    assert one["joinLink"] == "abcdefgh12.keykeykeykeykeykeykeyZZ", one
    assert one["browserUrl"] == "https://relay.test:7475/#abcdefgh12.keykeykeykeykeykeykeyZZ", one
    assert one["qr"].startswith("data:image/png;base64,"), one

    # The pane endpoint returns the session's terminal output.
    pane = machine.succeed(api(f"{API}/{sid}/pane?strip=1"))
    assert "fake omp ready" in pane, pane

    # Unknown id → 404.
    code = machine.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -H 'Authorization: Bearer {KEY}' {API}/nope"
    )
    assert code == "404", f"unknown id should be 404, got {code}"

    # The covibe CLI sees the same live session from the shared spool.
    machine.succeed("su vibe -c 'COVIBE_STATE_DIR=/run/covibe covibe list' | grep -q vmproj")

    # Killing the zellij session ends the pane; the dashboard prunes it.
    machine.succeed(zellij("kill-session covibe"))
    machine.wait_until_succeeds(api(API) + " | grep -q '^\[\]'", timeout=30)
  '';
}
