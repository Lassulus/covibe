self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.covibe;
  d = cfg.dashboard;
  # Absolute omp path so covibe execs the (patched) omp regardless of the tmux
  # server's PATH — a pre-existing tmux server started before a deploy
  # otherwise resolves "omp" to the old binary, which ignores OMP_COLLAB_*.
  ompBin = if cfg.ompPackage != null then "${cfg.ompPackage}/bin/omp" else cfg.omp;
  # The peer-to-peer sidecar, and only when asked for: it is what makes a session
  # reachable without the dashboard in the data path, and with iroh's default
  # relays that also means outbound connections to third-party infrastructure. So
  # it is opt-in, and p2p.relay points it at your own relay instead.
  p2pBin = if cfg.p2p.enable && cfg.p2p.package != null then lib.getExe cfg.p2p.package else null;
  # When a self-hosted collab-web root is set, browser links point at covibe's
  # own /c/ instead of the default my.omp.sh client.
  webClientUrl =
    if cfg.webRoot != "" && cfg.webClient == "https://my.omp.sh" && cfg.relayHost != "" then
      "https://${cfg.relayHost}/c"
    else
      cfg.webClient;

  # The user directory / member lists live next to, not inside, the tmpfs spool.
  accessDir = builtins.dirOf d.accessFile;

  # COVIBE_* environment shared by the dashboard service and — when
  # installGlobally is set — every interactive `covibe start`/`session` so both
  # sides agree on the spool directory, relay and tmux session name.
  sharedEnv = lib.filterAttrs (_: v: v != null && v != "") {
    COVIBE_STATE_DIR = cfg.stateDir;
    COVIBE_OMP = ompBin;
    COVIBE_WEB_ROOT = cfg.webRoot;
    COVIBE_RELAY_HOST = cfg.relayHost;
    COVIBE_WEB_CLIENT = webClientUrl;
    COVIBE_LOCAL_RELAY = "ws://" + d.addr;
    COVIBE_MUX_SESSION = cfg.muxSession;
    COVIBE_P2P = p2pBin;
    COVIBE_P2P_RELAY = lib.optionalString cfg.p2p.enable cfg.p2p.relay;
    OMP_AUTH_BROKER_URL = cfg.authBrokerUrl;
  };

  dashboardEnv =
    sharedEnv
    // lib.filterAttrs (_: v: v != null && v != "") {
      COVIBE_ADDR = d.addr;
      COVIBE_WORKSPACE = d.workspaceRoot;
      COVIBE_MAX_SESSIONS = toString d.maxSessions;
      COVIBE_MODELS = lib.concatStringsSep "," d.models;
      COVIBE_API_KEYS_FILE = d.apiKeysFile;
      COVIBE_USER_KEYS = lib.concatStringsSep "," d.userKeys;
      COVIBE_USER_KEYS_FILE = d.userKeysFile;
      COVIBE_OIDC_ISSUER = d.oidc.issuer;
      COVIBE_OIDC_CLIENT_ID = d.oidc.clientId;
      COVIBE_OIDC_REDIRECT_URL = d.oidc.redirectUrl;
      COVIBE_OIDC_SCOPES = lib.concatStringsSep " " d.oidc.scopes;
      COVIBE_ALLOW_EMAILS = lib.concatStringsSep "," d.allow.emails;
      COVIBE_ALLOW_DOMAINS = lib.concatStringsSep "," d.allow.domains;
      COVIBE_ALLOW_SUBS = lib.concatStringsSep "," d.allow.subs;
      COVIBE_ADMINS = lib.concatStringsSep "," d.admins;
      COVIBE_ACCESS_FILE = d.accessFile;
      COVIBE_NO_AUTH = if d.noAuth then "1" else "0";
      COVIBE_INSECURE = if d.insecure then "1" else "0";
    }
    // d.extraSettings;

  # tmux is the only backend: every session runs on its own tmux socket under
  # <stateDir>/tmux, which is also what the browser terminal drives over
  # control mode.
  dashboardPath = [ pkgs.tmux ] ++ lib.optional (cfg.ompPackage != null) cfg.ompPackage;
in
{
  imports = [
    # Deliberate breaking change: covibe no longer has a second multiplexer
    # backend, so a stale `mux` definition must fail loudly instead of being
    # silently ignored. socketDir went with it — only the dropped backend
    # needed a shared control-socket directory.
    (lib.mkRemovedOptionModule [ "services" "covibe" "mux" ] ''
      tmux is the only multiplexer covibe supports; there is no backend to
      choose. Delete this option.
    '')
    (lib.mkRemovedOptionModule [ "services" "covibe" "socketDir" ] ''
      covibe runs one tmux server per session, on its own socket under
      <stateDir>/tmux, and joins it with `covibe attach <name>` — there is no
      shared control-socket directory left to point anywhere. Delete this
      option.
    '')
  ];

  options.services.covibe = {
    enable = lib.mkEnableOption "covibe co-vibing sessions";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.covibe;
      defaultText = lib.literalExpression "covibe.packages.\${system}.covibe";
      description = "The covibe package to use.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      description = ''
        Login user that hosts the omp sessions and runs the dashboard. The
        dashboard must run as the same user that launches `covibe start` so it
        can read and prune that user's session spool.
      '';
      example = "alice";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "users";
      description = "Group owning the spool directory.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/run/covibe";
      description = "Session spool directory shared by session wrappers and the dashboard.";
    };

    relayHost = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "covibe.lassul.us";
      description = ''
        Public host collab clients connect to (guest links + collab-web).
        covibe serves the omp-compatible relay at /r/ on this host and embeds
        it in each session's join/browser links. Required for collab.
      '';
    };

    webClient = lib.mkOption {
      type = lib.types.str;
      default = "https://my.omp.sh";
      example = "https://covibe.lassul.us";
      description = ''
        collab-web client base for browser links: the deep link is
        <webClient>/#<relayHost>/r/<room>.<key>. Defaults to omp's hosted
        client (content-blind; the room key stays in the URL fragment). Point
        at a self-hosted collab-web build to drop the external dependency.
      '';
    };

    webRoot = lib.mkOption {
      type = lib.types.str;
      default = if cfg.ompPackage != null then "${cfg.ompPackage}/share/collab-web" else "";
      defaultText = lib.literalExpression "\"\${ompPackage}/share/collab-web\"";
      example = "/nix/store/…-omp/share/collab-web";
      description = ''
        Directory of a built collab-web static SPA (base path /c/) to self-host
        at <host>/c/. When set, browser links default to https://<relayHost>/c
        and nothing loads from my.omp.sh. Defaults to the SPA shipped by the
        patched omp package, so a stock deployment is fully self-hosted; set to
        "" to fall back to omp's hosted client.
      '';
    };

    muxSession = lib.mkOption {
      type = lib.types.str;
      default = "covibe";
      description = "tmux session name that covibe windows are opened in.";
    };

    omp = lib.mkOption {
      type = lib.types.str;
      default = "omp";
      description = "omp binary name/path launched inside each session.";
    };

    ompPackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.omp;
      defaultText = lib.literalExpression "covibe.packages.\${system}.omp";
      description = ''
        omp package placed on the dashboard service PATH and launched inside each
        session (COVIBE_OMP points at its binary by absolute path, so sessions do
        not depend on the tmux server's PATH). Defaults to covibe's patched omp:
        env-driven supervised collab hosting plus the self-hosted collab-web SPA.
        null leaves omp resolution to PATH.
      '';
    };

    p2p = {
      enable = lib.mkEnableOption ''
        peer-to-peer session terminals. Each session mints a read-write and a
        read-only iroh ticket, printed in its pane and recorded under
        <stateDir>/p2p; `covibe attach --ticket <t>` then reaches the session
        directly, with the dashboard in neither the data path nor the
        authorization decision. A ticket is the whole credential and lives exactly
        as long as the session it names, so it cannot be revoked individually —
        ending the session is what invalidates it
      '';

      package = lib.mkOption {
        type = lib.types.nullOr lib.types.package;
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.covibe-p2p;
        defaultText = lib.literalExpression "covibe.packages.\${system}.covibe-p2p";
        description = "The covibe-p2p sidecar holding each session's iroh endpoints.";
      };

      relay = lib.mkOption {
        type = lib.types.str;
        default = "";
        example = "https://relay.example.com";
        description = ''
          iroh relay used for rendezvous and as the fallback path when a direct
          connection cannot be established. Empty uses iroh's own public relays,
          which means tickets reference third-party infrastructure; run
          `iroh-relay` yourself and set this to keep that in-house. Traffic is
          end-to-end encrypted either way, so a relay sees only ciphertext.
        '';
      };
    };

    authBrokerUrl = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "https://broker.example.com:8765";
      description = ''
        Optional omp auth-broker/gateway URL exported to launched sessions as
        OMP_AUTH_BROKER_URL, so co-vibing omp instances resolve provider
        credentials through the broker instead of holding local tokens.
      '';
    };

    installGlobally = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Install covibe system-wide and export the shared COVIBE_* environment so
        interactive `covibe start`/`session` use the same spool, relay and tmux
        session name as the dashboard.
      '';
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ pkgs.tmux ];
      defaultText = lib.literalExpression "[ pkgs.tmux ]";
      description = "Extra packages (tmux, etc.) to install alongside covibe.";
    };

    dashboard = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Run the covibe dashboard as a systemd service.";
      };

      addr = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:8770";
        description = "Listen address for the dashboard.";
      };

      workspaceRoot = lib.mkOption {
        type = lib.types.str;
        default = "";
        example = "/home/alice/covibe";
        description = ''
          Enable web-initiated session creation, clamped inside this directory.
          The dashboard form creates <workspaceRoot>/<name> (or a supplied
          subdir) and launches an omp covibe session there. Empty disables
          creation from the web UI.
        '';
      };

      maxSessions = lib.mkOption {
        type = lib.types.int;
        default = 32;
        description = "Cap on concurrent live sessions creatable via the API/UI; 0 disables the cap.";
      };

      models = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [
          "anthropic/claude-opus-4-6"
          "openai/gpt-5.3-codex"
        ];
        description = ''
          Model ids offered as a datalist in the create form (COVIBE_MODELS).
          Empty leaves the model field as free-text.
        '';
      };

      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open the dashboard TCP port in the firewall.";
      };

      noAuth = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Disable OIDC entirely. Loopback/dev only — never expose this.";
      };

      insecure = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Allow session cookies over plain http (localhost/dev only).";
      };

      oidc = {
        issuer = lib.mkOption {
          type = lib.types.str;
          default = "";
          example = "https://auth.example.com/realms/main";
          description = "OIDC issuer URL (discovery via /.well-known/openid-configuration).";
        };
        clientId = lib.mkOption {
          type = lib.types.str;
          default = "";
          description = "OIDC client id.";
        };
        redirectUrl = lib.mkOption {
          type = lib.types.str;
          default = "";
          example = "https://covibe.example.com/auth/callback";
          description = "OIDC redirect URL; must resolve to the dashboard's /auth/callback.";
        };
        scopes = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [
            "openid"
            "profile"
            "email"
          ];
          description = "OIDC scopes requested.";
        };
      };

      allow = {
        emails = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [ ];
          description = "Allowed emails. Empty + empty domains/subs allows any authenticated user.";
        };
        domains = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [ ];
          description = "Allowed email domains.";
        };
        subs = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [ ];
          description = "Allowed OIDC subject ids.";
        };
      };

      admins = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [ "lassulus" ];
        description = ''
          Admin users, matched against the OIDC email, subject id or
          preferred_username. Admins see every session and may manage the
          members of any session. In noAuth mode everyone is an admin.
        '';
      };

      accessFile = lib.mkOption {
        type = lib.types.str;
        default = "/var/lib/covibe/access.json";
        description = ''
          JSON file holding the user directory and the per-session member
          lists. Must live outside the tmpfs spool (stateDir) so users and
          session memberships survive a reboot.
        '';
      };

      apiKeysFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
          File of API keys (one per line, `#` comments, optional `label:` prefix)
          authorizing the machine-facing /api/v1 REST surface. Inline keys can
          also be supplied as COVIBE_API_KEYS via environmentFile.
        '';
      };

      userKeys = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [ "alice@example.com:CHANGEME" ];
        description = ''
          Per-user API keys, one `user:token` per entry, where user is the OIDC
          email, subject id or preferred_username (matched the same way admins
          and session members are). A request carrying such a token acts AS
          that user: a session registered with it is owned by them and appears
          in their dashboard, unlike a keyless registration, which is owned by
          nobody and is therefore visible to admins only.

          This is not apiKeysFile: those are machine/admin credentials with
          access to every session, while a user key acts as exactly one user.

          Tokens listed here land in the world-readable Nix store and in the
          unit's environment, so userKeysFile is the production choice.
        '';
      };

      userKeysFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
          File of per-user API keys (one `user:token` per line, `#` comments)
          with the same meaning as userKeys but kept out of the Nix store —
          point it at a secret-manager path readable by the service user.
          Inline pairs can also be supplied as COVIBE_USER_KEYS via
          environmentFile.
        '';
      };

      environmentFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
          systemd EnvironmentFile with secrets, e.g.
          COVIBE_OIDC_CLIENT_SECRET=... and COVIBE_COOKIE_SECRET=... . The cookie
          secret keeps sessions valid across restarts; set it in production.
        '';
      };

      extraSettings = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        description = "Extra COVIBE_* environment variables for the dashboard.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion =
          cfg.dashboard.enable
          -> (d.noAuth || (d.oidc.issuer != "" && d.oidc.clientId != "" && d.oidc.redirectUrl != ""));
        message = "services.covibe.dashboard: set dashboard.oidc.{issuer,clientId,redirectUrl} or dashboard.noAuth = true.";
      }
      {
        # A pair without a colon names no user, so the token would silently
        # authorize nobody: fail at eval time instead.
        assertion = lib.all (lib.hasInfix ":") d.userKeys;
        message = "services.covibe.dashboard.userKeys: every entry must be of the form user:token.";
      }
    ];

    environment.systemPackages = lib.mkIf cfg.installGlobally ([ cfg.package ] ++ cfg.extraPackages);
    environment.variables = lib.mkIf cfg.installGlobally sharedEnv;

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 ${cfg.user} ${cfg.group} -"
      # covibe's own tmux sockets (<stateDir>/tmux/<owner>-<id>.sock, one
      # server per session); pre-created so a fresh boot has it before the
      # first session.
      "d ${cfg.stateDir}/tmux 0700 ${cfg.user} ${cfg.group} -"
    ]
    ++ lib.optional (d.workspaceRoot != "") "d ${d.workspaceRoot} 0700 ${cfg.user} ${cfg.group} -"
    ++ lib.optional (d.accessFile != "") "d ${accessDir} 0700 ${cfg.user} ${cfg.group} -";

    systemd.services.covibe-dashboard = lib.mkIf cfg.dashboard.enable {
      description = "covibe co-vibing session dashboard";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      environment = dashboardEnv;
      # The tmux binary (and optionally omp) must be reachable so the dashboard
      # can open new sessions.
      path = dashboardPath;
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} serve";
        User = cfg.user;
        Group = cfg.group;
        Restart = "on-failure";
        RestartSec = 2;
        EnvironmentFile = lib.mkIf (d.environmentFile != null) [ d.environmentFile ];
        # Kill only the dashboard process on stop/restart, never the whole
        # control group: the tmux servers and the omp sessions they host must
        # outlive dashboard restarts. Those sessions are full-power dev shells,
        # so the service is intentionally not filesystem-sandboxed.
        KillMode = "process";
        NoNewPrivileges = false;
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf (cfg.dashboard.enable && d.openFirewall) [
      (lib.toInt (lib.last (lib.splitString ":" d.addr)))
    ];
  };
}
