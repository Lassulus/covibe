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

  # COVIBE_* environment shared by the dashboard service and — when
  # installGlobally is set — every interactive `covibe start`/`session` so both
  # sides agree on the spool directory, relay and multiplexer.
  sharedEnv = lib.filterAttrs (_: v: v != null && v != "") {
    COVIBE_STATE_DIR = cfg.stateDir;
    COVIBE_RELAY_HOST = cfg.relayHost;
    COVIBE_WEB_CLIENT = cfg.webClient;
    COVIBE_LOCAL_RELAY = "ws://" + d.addr;
    COVIBE_MUX = cfg.mux;
    COVIBE_MUX_SESSION = cfg.muxSession;
    OMP_AUTH_BROKER_URL = cfg.authBrokerUrl;
    # Pin the multiplexer socket dirs so dashboard-created sessions and the
    # user's interactive `zellij attach` / `tmux attach` share one server.
    ZELLIJ_SOCKET_DIR = "${cfg.socketDir}/zellij";
    TMUX_TMPDIR = cfg.socketDir;
  };

  dashboardEnv =
    sharedEnv
    // lib.filterAttrs (_: v: v != null && v != "") {
      COVIBE_ADDR = d.addr;
      COVIBE_WORKSPACE = d.workspaceRoot;
      COVIBE_MAX_SESSIONS = toString d.maxSessions;
      COVIBE_OMP = cfg.omp;
      COVIBE_API_KEYS_FILE = d.apiKeysFile;
      COVIBE_OIDC_ISSUER = d.oidc.issuer;
      COVIBE_OIDC_CLIENT_ID = d.oidc.clientId;
      COVIBE_OIDC_REDIRECT_URL = d.oidc.redirectUrl;
      COVIBE_OIDC_SCOPES = lib.concatStringsSep " " d.oidc.scopes;
      COVIBE_ALLOW_EMAILS = lib.concatStringsSep "," d.allow.emails;
      COVIBE_ALLOW_DOMAINS = lib.concatStringsSep "," d.allow.domains;
      COVIBE_ALLOW_SUBS = lib.concatStringsSep "," d.allow.subs;
      COVIBE_NO_AUTH = if d.noAuth then "1" else "0";
      COVIBE_INSECURE = if d.insecure then "1" else "0";
    }
    // d.extraSettings;

  muxPackage = if cfg.mux == "tmux" then pkgs.tmux else pkgs.zellij;
  dashboardPath = [ muxPackage ] ++ lib.optional (cfg.ompPackage != null) cfg.ompPackage;
in
{
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

    mux = lib.mkOption {
      type = lib.types.enum [
        "zellij"
        "tmux"
      ];
      default = "zellij";
      description = "Terminal multiplexer covibe launches sessions in.";
    };

    muxSession = lib.mkOption {
      type = lib.types.str;
      default = "covibe";
      description = "Multiplexer session name that covibe tabs are opened in.";
    };

    socketDir = lib.mkOption {
      type = lib.types.str;
      default = cfg.stateDir;
      defaultText = lib.literalExpression "config.services.covibe.stateDir";
      description = ''
        Directory pinning the multiplexer control sockets (ZELLIJ_SOCKET_DIR =
        <socketDir>/zellij, TMUX_TMPDIR = <socketDir>). Exported to both the
        dashboard service and interactive shells so web-created sessions and the
        user's `zellij attach`/`tmux attach` share one server.
      '';
    };

    omp = lib.mkOption {
      type = lib.types.str;
      default = "omp";
      description = "omp binary name/path launched inside each session.";
    };

    ompPackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = "Optional omp package placed on the dashboard service PATH so it can launch sessions.";
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
        interactive `covibe start`/`session` use the same spool, relay and mux
        as the dashboard.
      '';
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ (if cfg.mux == "tmux" then pkgs.tmux else pkgs.zellij) ];
      defaultText = lib.literalExpression "[ pkgs.zellij ]";
      description = "Extra packages (the multiplexer, etc.) to install alongside covibe.";
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

      apiKeysFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
          File of API keys (one per line, `#` comments, optional `label:` prefix)
          authorizing the machine-facing /api/v1 REST surface. Inline keys can
          also be supplied as COVIBE_API_KEYS via environmentFile.
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
    ];

    environment.systemPackages = lib.mkIf cfg.installGlobally ([ cfg.package ] ++ cfg.extraPackages);
    environment.variables = lib.mkIf cfg.installGlobally sharedEnv;

    systemd.tmpfiles.rules = lib.unique (
      [
        "d ${cfg.stateDir} 0700 ${cfg.user} ${cfg.group} -"
        "d ${cfg.socketDir} 0700 ${cfg.user} ${cfg.group} -"
        "d ${cfg.socketDir}/zellij 0700 ${cfg.user} ${cfg.group} -"
      ]
      ++ lib.optional (d.workspaceRoot != "") "d ${d.workspaceRoot} 0700 ${cfg.user} ${cfg.group} -"
    );

    systemd.services.covibe-dashboard = lib.mkIf cfg.dashboard.enable {
      description = "covibe co-vibing session dashboard";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      environment = dashboardEnv;
      # The multiplexer client + server binaries (and optionally omp) must be
      # reachable so the dashboard can open new sessions.
      path = dashboardPath;
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} serve";
        User = cfg.user;
        Group = cfg.group;
        Restart = "on-failure";
        RestartSec = 2;
        EnvironmentFile = lib.mkIf (d.environmentFile != null) [ d.environmentFile ];
        # Kill only the dashboard process on stop/restart, never the whole
        # control group: the mux server and the omp sessions it spawned must
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
