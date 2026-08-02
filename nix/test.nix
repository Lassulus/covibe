# NixOS VM test: boots the covibe module, then drives the dashboard over HTTP
# to prove the full path — service up, web-initiated session creation, tmux
# launch, and /collab-link capture — works end to end.
#
# The dashboard creates a detached tmux session on a covibe-owned socket
# (<stateDir>/tmux/<owner>-<id>.sock) and opens the omp pane in it, with no
# client attached — the same socket the browser terminal drives over control
# mode. omp is not packaged, so a fake `omp` stands in: it emits a realistic
# /collab banner when the session wrapper auto-types /collab, which is exactly
# what the link sniffer consumes.
# Auth runs in no-auth mode so the test needs no OIDC provider (the OIDC path is
# covered by unit tests).
# It also covers the remote path: a wrapper registering over REST with a
# per-user key (dashboard.userKeys) gets a session owned by that user, while a
# keyless registration stays unowned and thus admin-only.
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
        webClient = "https://relay.test:7475";
        ompPackage = fakeOmp;
        dashboard = {
          addr = "127.0.0.1:8770";
          noAuth = true;
          insecure = true;
          workspaceRoot = "/home/vibe/covibe";
          # Exercise the machine-facing REST surface with a real API key.
          extraSettings.COVIBE_API_KEYS = "test-key";
          # Per-user key: a request carrying it acts as vibe@example.com, so a
          # session registered with it is owned by them (not admin-only).
          userKeys = [ "vibe@example.com:usertoken123" ];
        };
      };

      environment.systemPackages = [ pkgs.curl ];
      virtualisation.graphics = false;
    };

  testScript = ''
    import json

    KEY = "test-key"
    API = "http://127.0.0.1:8770/api/v1/sessions"

    def tmux(sock, cmd):
        return f"su vibe -c 'tmux -S {sock} {cmd}'"

    def api(args):
        return f"curl -sf -H 'Authorization: Bearer {KEY}' {args}"

    start_all()
    machine.wait_for_unit("covibe-dashboard.service")
    machine.wait_for_open_port(8770)

    # Liveness (public, no key) + security headers on every response.
    machine.succeed("curl -sf http://127.0.0.1:8770/healthz")
    headers = machine.succeed("curl -sfi http://127.0.0.1:8770/healthz")
    assert "x-content-type-options: nosniff" in headers.lower(), headers
    assert "x-frame-options: deny" in headers.lower(), headers
    # The HTML dashboard carries a nonce-based CSP.
    idx = machine.succeed("curl -sfi http://127.0.0.1:8770/")
    assert "content-security-policy: default-src 'self'; script-src 'nonce-" in idx.lower(), idx

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

    # The spool record names the covibe-owned tmux socket the session's server
    # listens on (<stateDir>/tmux/<owner>-<id>.sock, 0600) — the socket the
    # browser terminal drives over tmux control mode.
    rec = json.loads(machine.succeed(f"cat /run/covibe/{sid}.json"))
    sock = rec["muxSocket"]
    assert sock.startswith("/run/covibe/tmux/"), rec
    machine.wait_until_succeeds(f"test -S {sock}", timeout=30)

    # A backgrounded tmux session on that socket hosts the pane.
    machine.wait_until_succeeds(tmux(sock, "list-sessions") + " | grep -q '^covibe:'", timeout=30)
    sessions = machine.succeed(tmux(sock, "list-sessions"))
    assert "covibe:" in sessions, sessions

    # GET /sessions/<id> exposes the omp remote key once /collab is captured.
    machine.wait_until_succeeds(
        api(f"{API}/{sid}") + " | grep -q '\"joinLink\":\"abcdefgh12.keykeykeykeykeykeykeyZZ\"'",
        timeout=60,
    )
    one = json.loads(machine.succeed(api(f"{API}/{sid}")))
    assert one["name"] == "vmproj", one
    assert one["status"] == "live", one
    # Sessions are tmux-backed, so they expose the browser terminal, writable
    # for their owner.
    assert one["hasTerminal"], one
    assert one["canWrite"], one
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

    # Killing the tmux session ends the pane; the dashboard prunes it.
    machine.succeed(tmux(sock, "kill-session -t covibe"))
    machine.wait_until_succeeds(api(API) + " | grep -q '^\[\]'", timeout=30)

    # --- Remote registration ------------------------------------------------
    # A wrapper on another machine registers over REST. With a per-user key the
    # session is attributed to that user; without one it belongs to nobody.
    # noAuth makes the *keyless* caller an admin, but a user key never inherits
    # that: it is matched against dashboard.admins, which is empty here.
    USER = "usertoken123"

    def uapi(args):
        return f"curl -sf -H 'Authorization: Bearer {USER}' {args}"

    def register(token, name):
        hdr = f"-H 'Authorization: Bearer {token}' " if token else ""
        code = machine.succeed(
            f"curl -s -o /tmp/reg.json -w '%{{http_code}}' -X POST {hdr}"
            f"-H 'Content-Type: application/json' -d '{{\"name\":\"{name}\"}}' {API}/register"
        )
        return json.loads(machine.succeed("cat /tmp/reg.json")), code

    owned, code = register(USER, "laptop")
    assert code == "201", f"user-key register should be 201, got {code}"
    oid = owned["id"]

    # Its owner sees it by id and in their list, attributed to them.
    mine = json.loads(machine.succeed(uapi(f"{API}/{oid}")))
    assert mine["name"] == "laptop", mine
    assert "vibe@example.com" in mine.get("owner", ""), mine
    listed = machine.succeed(uapi(API))
    assert oid in listed, listed

    # The same registration without a token is owned by nobody: admins still
    # see it, a plain user key does not — neither in the list nor by id.
    anon, code = register(None, "orphan")
    assert code == "201", f"keyless register should be 201, got {code}"
    aid = anon["id"]
    admin_list = machine.succeed(api(API))
    assert aid in admin_list, admin_list
    user_list = machine.succeed(uapi(API))
    assert aid not in user_list, user_list
    code = machine.succeed(
        "curl -s -o /dev/null -w '%{http_code}' "
        f"-H 'Authorization: Bearer {USER}' {API}/{aid}"
    )
    assert code == "404", f"unowned session should be 404 for a plain user, got {code}"
  '';
}
