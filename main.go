// Command covibe runs a co-vibing setup: it launches omp sessions inside a
// terminal multiplexer, streams each session to a built-in collab relay via an
// omp extension, and serves an OIDC-protected dashboard of live sessions with
// scannable QR codes plus a browser viewer.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lassulus/covibe/internal/access"
	"github.com/lassulus/covibe/internal/dashboard"
	"github.com/lassulus/covibe/internal/launch"
	"github.com/lassulus/covibe/internal/session"
	"github.com/lassulus/covibe/internal/spool"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "session":
		err = cmdSession(os.Args[2:])
	case "start":
		err = cmdStart(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("covibe", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "covibe:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `covibe — co-vibing sessions for omp

usage:
  covibe start   <name> [flags]     open an omp session in a mux tab and share it
  covibe session         [flags]    (internal) pty-proxy run by the mux; wraps omp
  covibe list            [flags]    list live sessions
  covibe serve           [flags]    run the OIDC-protected session dashboard
  covibe version

run 'covibe <command> -h' for flags.
`)
}

// env returns the first non-empty of the environment variable or the fallback.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt reads an int environment variable, falling back on unset/invalid.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// cmdSession is the pane process: proxy omp and stream it to covibe's relay.
func cmdSession(args []string) error {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	id := fs.String("id", "", "stable session id (generated if empty)")
	name := fs.String("name", "", "session display name")
	dir := fs.String("dir", "", "working directory for omp")
	omp := fs.String("omp", env("COVIBE_OMP", "omp"), "omp binary")
	relayHost := fs.String("relay-host", env("COVIBE_RELAY_HOST", ""), "public host for guest links, e.g. covibe.lassul.us")
	webClient := fs.String("web-client", env("COVIBE_WEB_CLIENT", "https://my.omp.sh"), "collab-web client base for browser links")
	localRelay := fs.String("local-relay", env("COVIBE_LOCAL_RELAY", ""), "ws(s):// relay base the omp host connects to")
	muxName := fs.String("mux", env("COVIBE_MUX", ""), "multiplexer label (zellij|tmux)")
	muxSession := fs.String("mux-session", env("COVIBE_MUX_SESSION", ""), "multiplexer session name")
	muxSocket := fs.String("mux-socket", env("COVIBE_MUX_SOCKET", ""), "tmux server socket this session runs on (recorded for the dashboard terminal)")
	stateDir := fs.String("state-dir", "", "spool directory")
	model := fs.String("model", env("COVIBE_MODEL", ""), "omp model selector (optional)")
	thinking := fs.String("thinking", env("COVIBE_THINKING", ""), "omp thinking level (optional)")
	dashboardURL := fs.String("dashboard", env("COVIBE_DASHBOARD", ""), "dashboard base URL for remote registration; enables remote mode")
	token := fs.String("token", env("COVIBE_TOKEN", ""), "per-user dashboard API key; makes the session owned and enables its web terminal")
	_ = fs.Parse(args)

	if *dir == "" {
		if wd, err := os.Getwd(); err == nil {
			*dir = wd
		}
	}
	// A session in ~/src/covibe is "covibe", not the literal "session": the name
	// is the dashboard label, and the wrapper no longer has to compute it.
	if *name == "" {
		if base := filepath.Base(*dir); base != "" && base != "." && base != string(os.PathSeparator) {
			*name = base
		}
	}
	cfg := session.Config{
		ID:         *id,
		Name:       orDefault(*name, "session"),
		Dir:        *dir,
		OmpBin:     *omp,
		OmpArgs:    fs.Args(),
		Model:      *model,
		Thinking:   *thinking,
		RelayHost:  *relayHost,
		WebClient:  *webClient,
		LocalRelay: *localRelay,
		Mux:        *muxName,
		MuxSession: *muxSession,
		MuxSocket:  *muxSocket,
		Token:      *token,
	}
	if *dashboardURL != "" {
		remote := session.NewRemoteSink(*dashboardURL)
		remote.Token = *token
		cfg.Sink = remote
	} else {
		store, err := spool.Open(*stateDir)
		if err != nil {
			return err
		}
		cfg.Store = store
	}
	return session.Run(cfg)
}

// cmdStart asks the multiplexer to open a tab running covibe session.
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	name := fs.String("name", "", "session name (also positional)")
	dir := fs.String("dir", "", "working directory (default: cwd)")
	muxName := fs.String("mux", env("COVIBE_MUX", "tmux"), "multiplexer: zellij|tmux")
	muxSession := fs.String("session", env("COVIBE_MUX_SESSION", "covibe"), "multiplexer session name")
	muxSocket := fs.String("mux-socket", env("COVIBE_MUX_SOCKET", ""), "tmux server socket (default <state-dir>/tmux/local.sock)")
	relayHost := fs.String("relay-host", env("COVIBE_RELAY_HOST", ""), "public host for guest links")
	webClient := fs.String("web-client", env("COVIBE_WEB_CLIENT", "https://my.omp.sh"), "collab-web client base")
	localRelay := fs.String("local-relay", env("COVIBE_LOCAL_RELAY", ""), "ws(s):// relay base the omp host connects to")
	omp := fs.String("omp", env("COVIBE_OMP", "omp"), "omp binary")
	stateDir := fs.String("state-dir", "", "spool directory")
	dryRun := fs.Bool("dry-run", false, "print the mux command instead of running it")
	model := fs.String("model", env("COVIBE_MODEL", ""), "omp model selector (optional)")
	thinking := fs.String("thinking", env("COVIBE_THINKING", ""), "omp thinking level (optional)")
	// Allow "covibe start <name> [flags]": pop a leading non-flag arg as the
	// name so flags after it are still parsed (Go's flag stops at the first
	// positional otherwise).
	var posName string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		posName, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if *name == "" {
		*name = posName
	}
	if *name == "" && fs.NArg() > 0 {
		*name = fs.Arg(0)
	}
	if *name == "" {
		return fmt.Errorf("session name required")
	}
	if *dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		*dir = wd
	}
	opts := launch.Options{
		Name:       *name,
		Dir:        *dir,
		Mux:        *muxName,
		MuxSession: *muxSession,
		MuxSocket:  *muxSocket,
		RelayHost:  *relayHost,
		WebClient:  *webClient,
		LocalRelay: *localRelay,
		Omp:        *omp,
		StateDir:   *stateDir,
		Model:      *model,
		Thinking:   *thinking,
	}
	// A CLI-started session belongs to the host operator, so it goes on the
	// "local" socket: one tmux server, reachable by the dashboard's terminal.
	if opts.MuxSocket == "" && *muxName == "tmux" {
		store, err := spool.Open(*stateDir)
		if err != nil {
			return err
		}
		if opts.MuxSocket, err = store.TmuxSocket(""); err != nil {
			return err
		}
	}
	if *dryRun {
		argv, err := launch.Command(opts)
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(argv, " "))
		return nil
	}
	sess, err := launch.Launch(opts)
	if err != nil {
		return err
	}
	fmt.Printf("started %q in %s session %q\n", *name, *muxName, sess)
	return nil
}

// cmdList prints the live sessions.
func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	stateDir := fs.String("state-dir", "", "spool directory")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)

	store, err := spool.Open(*stateDir)
	if err != nil {
		return err
	}
	recs, err := store.Live(30 * time.Second)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(recs)
	}
	if len(recs) == 0 {
		fmt.Println("no live sessions")
		return nil
	}
	for _, r := range recs {
		link := r.BrowserURL
		if link == "" {
			link = "(starting)"
		}
		fmt.Printf("%-8s %-16s %-8s %s\n\t%s\n", r.Status, r.Name, r.Mux, r.Dir, link)
	}
	return nil
}

// cmdServe runs the dashboard.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("COVIBE_ADDR", "127.0.0.1:8770"), "listen address")
	stateDir := fs.String("state-dir", "", "spool directory")
	relayHost := fs.String("relay-host", env("COVIBE_RELAY_HOST", ""), "public host for guest collab links, e.g. covibe.lassul.us")
	webClient := fs.String("web-client", env("COVIBE_WEB_CLIENT", "https://my.omp.sh"), "collab-web client base for browser links")
	noAuth := fs.Bool("no-auth", os.Getenv("COVIBE_NO_AUTH") == "1", "disable OIDC (loopback dev only)")
	insecure := fs.Bool("insecure", os.Getenv("COVIBE_INSECURE") == "1", "allow cookies over plain http")
	issuer := fs.String("oidc-issuer", env("COVIBE_OIDC_ISSUER", ""), "OIDC issuer URL")
	clientID := fs.String("oidc-client-id", env("COVIBE_OIDC_CLIENT_ID", ""), "OIDC client id")
	clientSecret := fs.String("oidc-client-secret", env("COVIBE_OIDC_CLIENT_SECRET", ""), "OIDC client secret (optional for PKCE public clients)")
	redirect := fs.String("oidc-redirect-url", env("COVIBE_OIDC_REDIRECT_URL", ""), "OIDC redirect URL (…/auth/callback)")
	scopes := fs.String("oidc-scopes", env("COVIBE_OIDC_SCOPES", ""), "space-separated OIDC scopes")
	allowEmails := fs.String("allow-emails", env("COVIBE_ALLOW_EMAILS", ""), "comma-separated allowed emails")
	allowDomains := fs.String("allow-domains", env("COVIBE_ALLOW_DOMAINS", ""), "comma-separated allowed email domains")
	allowSubs := fs.String("allow-subs", env("COVIBE_ALLOW_SUBS", ""), "comma-separated allowed subject ids")
	admins := fs.String("admins", env("COVIBE_ADMINS", ""), "comma-separated admin users (email, sub or preferred_username) with full access")
	accessFile := fs.String("access-file", env("COVIBE_ACCESS_FILE", ""), "JSON file holding the user directory and per-session member lists (default <state-dir>/access.json)")
	workspace := fs.String("workspace", env("COVIBE_WORKSPACE", ""), "workspace root enabling web session creation (sessions clamped inside it)")
	muxName := fs.String("mux", env("COVIBE_MUX", "tmux"), "multiplexer for created sessions: zellij|tmux (the browser terminal needs tmux)")
	muxSession := fs.String("mux-session", env("COVIBE_MUX_SESSION", "covibe"), "multiplexer session name for created sessions")
	ompBin := fs.String("omp", env("COVIBE_OMP", "omp"), "omp binary for created sessions")
	apiKeys := fs.String("api-keys", env("COVIBE_API_KEYS", ""), "comma/space-separated API keys for the /api/v1 REST surface")
	apiKeysFile := fs.String("api-keys-file", env("COVIBE_API_KEYS_FILE", ""), "file of API keys (one per line, # comments) for /api/v1")
	userKeys := fs.String("user-keys", env("COVIBE_USER_KEYS", ""), "comma/space-separated user:token pairs; a request carrying the token acts as that user")
	userKeysFile := fs.String("user-keys-file", env("COVIBE_USER_KEYS_FILE", ""), "file of user:token pairs (one per line, # comments)")
	maxSessions := fs.Int("max-sessions", envInt("COVIBE_MAX_SESSIONS", 0), "cap on concurrent live sessions (0 = unlimited)")
	localRelay := fs.String("local-relay", env("COVIBE_LOCAL_RELAY", ""), "ws(s):// relay base the omp host connects to (default ws://<addr>)")
	webRoot := fs.String("web-root", env("COVIBE_WEB_ROOT", ""), "dir of collab-web static assets served at /c/ (self-hosted client)")
	models := fs.String("models", env("COVIBE_MODELS", ""), "comma-separated model ids for the create-form datalist")
	_ = fs.Parse(args)
	if *localRelay == "" {
		*localRelay = "ws://" + *addr
	}
	if *webRoot != "" && *webClient == "https://my.omp.sh" && *relayHost != "" {
		*webClient = "https://" + *relayHost + "/c"
	}

	store, err := spool.Open(*stateDir)
	if err != nil {
		return err
	}
	accessPath := *accessFile
	if accessPath == "" {
		accessPath = filepath.Join(store.Dir(), "access.json")
	}
	users, err := access.Open(accessPath)
	if err != nil {
		return err
	}
	auth, err := dashboard.NewAuthenticator(context.Background(), dashboard.OIDCConfig{
		Issuer:         *issuer,
		ClientID:       *clientID,
		ClientSecret:   *clientSecret,
		RedirectURL:    *redirect,
		Scopes:         splitFields(*scopes),
		AllowedEmails:  splitList(*allowEmails),
		AllowedDomains: splitList(*allowDomains),
		AllowedSubs:    splitList(*allowSubs),
		Admins:         splitList(*admins),
		CookieSecret:   []byte(os.Getenv("COVIBE_COOKIE_SECRET")),
		Insecure:       *insecure || *noAuth,
		NoAuth:         *noAuth,
	})
	if err != nil {
		return err
	}
	keys, err := dashboard.LoadAPIKeys(*apiKeys, *apiKeysFile)
	if err != nil {
		return err
	}
	// Machine keys reach every session; a user key acts as exactly one user, so
	// a laptop registering a session gets a session that user owns.
	if err := keys.AddUserKeys(*userKeys, *userKeysFile); err != nil {
		return err
	}
	cfg := dashboard.Config{
		Store:         store,
		Access:        users,
		Auth:          auth,
		RelayHost:     *relayHost,
		WebClient:     *webClient,
		WebRoot:       *webRoot,
		WorkspaceRoot: *workspace,
		APIKeys:       keys,
		MaxSessions:   *maxSessions,
		Models:        splitList(*models),
		OmpBin:        *ompBin,
	}
	if *workspace != "" {
		cfg.Create = func(sp dashboard.CreateSpec) error {
			// One tmux server per covibe user: the socket is both the isolation
			// boundary between users and the handle the dashboard drives to
			// stream the terminal.
			var socket string
			if *muxName == "tmux" {
				var err error
				if socket, err = store.TmuxSocket(sp.Owner); err != nil {
					return err
				}
			}
			_, err := launch.Launch(launch.Options{
				ID:         sp.ID,
				Name:       sp.Name,
				Dir:        sp.Dir,
				Model:      sp.Model,
				Thinking:   sp.Thinking,
				Mux:        *muxName,
				MuxSession: *muxSession,
				MuxSocket:  socket,
				RelayHost:  *relayHost,
				WebClient:  *webClient,
				LocalRelay: *localRelay,
				Omp:        *ompBin,
				StateDir:   store.Dir(),
			})
			return err
		}
	}
	srv := dashboard.NewServer(cfg)

	httpSrv := newHTTPServer(*addr, srv.Handler())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()

	mode := "OIDC"
	if *noAuth {
		mode = "NO-AUTH (dev)"
	}
	fmt.Printf("covibe dashboard on http://%s  [%s]  spool=%s  access=%s\n", *addr, mode, store.Dir(), accessPath)
	if err := httpSrv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
		return err
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitFields(s string) []string {
	return strings.Fields(s)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
}
