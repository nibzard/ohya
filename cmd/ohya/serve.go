// The serve subcommand: the hub side of device sync. It provisions and runs
// the self-hosted coordination plane in-process (internal/control — own
// database, generated config, embedded DERP relay), enrolls the hub's own
// transport node into it, and serves the sync endpoint on the encrypted
// tailnet: source registration, device batches, and the remote query and
// annotate frames of ADR 0003 through the same bounded engine as the stdio
// tools. Pairing, credentials and revocation are the T018 registry's
// (internal/registry); devices join with `ohya join` and one code from
// `ohya devices invite`, which bridges each invitation to a single-use
// coordinator preauth key.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/tailcat"
)

// hubAuthKeyTTL bounds a freshly minted hub-node admission key: it is
// consumed within seconds of a first start and ignored once the node state
// exists, so the bound only caps how long an unused key stays valid.
const hubAuthKeyTTL = 10 * time.Minute

// runServeCommand parses the serve flags and serves devices until the
// process receives SIGINT or SIGTERM.
func runServeCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	fs := flag.NewFlagSet("ohya serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	state := fs.String("state", "",
		"state `directory` for the device identity, registry, and coordination plane (default: the database directory)")
	controlListen := fs.String("control-listen", control.DefaultListenAddr,
		"host:port the coordination plane and its embedded DERP relay listen on")
	controlURL := fs.String("control-url", "",
		"coordination `URL` devices reach the hub at (default: http:// plus -control-listen; pass it explicitly when the hub is reachable from other machines)")
	stunListen := fs.String("stun-listen", control.DefaultSTUNAddr,
		"UDP host:port of the coordination plane's STUN helper for devices behind NAT")
	controlLogLevel := fs.String("control-log-level", "warn",
		"coordination plane log `level`: warn, info or debug")
	tailnetPort := fs.Int("tailnet-port", tailcat.DefaultHubPort,
		"tailnet `port` the sync endpoint listens on inside the transport")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	url := *controlURL
	if url == "" {
		host, _, err := net.SplitHostPort(*controlListen)
		if err != nil {
			return fmt.Errorf("parse -control-listen: %w", err)
		}
		if host == "0.0.0.0" || host == "::" || strings.Contains(host, ":") {
			return fmt.Errorf("-control-listen binds every interface; pass -control-url with the address devices use")
		}
		url = "http://" + *controlListen
	}
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, serveOptions{
		dbPath:          *db,
		stateDir:        stateDir,
		controlListen:   *controlListen,
		controlURL:      url,
		stunListen:      *stunListen,
		controlLogLevel: *controlLogLevel,
		tailnetPort:     *tailnetPort,
		stdout:          stdout,
	})
}

// serveOptions configures one serve run.
type serveOptions struct {
	dbPath          string
	stateDir        string
	controlListen   string
	controlURL      string
	stunListen      string
	controlLogLevel string
	tailnetPort     int
	stdout          io.Writer
}

// runServe wires the coordination plane, the transport node, the registry
// authenticator, the hub database and the bounded engine into the sync
// endpoint, then serves until ctx is done.
func runServe(ctx context.Context, o serveOptions) error {
	if err := os.MkdirAll(filepath.Dir(o.dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	// Coordination first: the hub's own node enrolls into it before devices
	// can. A second serve on the same state fails here, when its listeners
	// cannot bind.
	ctl, err := control.Provision(control.Options{
		Dir:        filepath.Join(o.stateDir, control.DirName),
		ServerURL:  o.controlURL,
		ListenAddr: o.controlListen,
		STUNAddr:   o.stunListen,
		BaseDomain: tailcat.DefaultBaseDomain,
		LogLevel:   o.controlLogLevel,
	})
	if err != nil {
		return err
	}
	defer ctl.Close()
	if err := ctl.Start(ctx); err != nil {
		return err
	}

	hub, err := store.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer hub.Close()
	deviceID, err := device.LoadOrCreate(o.stateDir)
	if err != nil {
		return err
	}
	reg, err := registry.Open(filepath.Join(o.stateDir, registry.FileName))
	if err != nil {
		return err
	}
	defer reg.Close()

	// The hub's own node: enrolled once with a single-use key, durable
	// afterwards. Restart without enrolled state mints a fresh key; a node
	// whose state was lost stays orphaned on the coordinator, so this is a
	// recovery point, not a silent identity change.
	st, err := ctl.State()
	if err != nil {
		return err
	}
	authKey := ""
	if !control.HubEnrolled(st) {
		user, err := control.EnsureUser(st)
		if err != nil {
			return err
		}
		authKey, err = control.MintAuthKey(st, user, hubAuthKeyTTL)
		if err != nil {
			return err
		}
	}
	node, err := tailcat.NewHub(tailcat.Options{
		Hostname:   tailcat.HubHostname,
		AuthKey:    authKey,
		ControlURL: ctl.URL(),
		StateDir:   filepath.Join(o.stateDir, "tailcat-hub"),
		Port:       o.tailnetPort,
	})
	if err != nil {
		return err
	}
	ln, err := node.Listen(ctx)
	if err != nil {
		return err
	}

	readers, err := hub.Reader()
	if err != nil {
		return err
	}
	defer readers.Close()
	engine := mcpserver.NewEngine(readers, hub.Writer(), mcpserver.EngineOptions{WriterID: deviceID})
	endpoint, err := hubsync.NewEndpoint(hub, &registry.Authenticator{DB: reg}, hubsync.Options{
		Remote: engine,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(o.stdout, "ohya: serving devices at %s (coordination %s)\n", node.Addr(), ctl.URL())
	fmt.Fprintf(o.stdout, "ohya: invite a device: ohya devices invite --state %s\n", o.stateDir)

	// Serve until the context ends — a signal, which the coordination plane's
	// own handler shuts down alongside — or the accept loop fails.
	err = endpoint.Serve(ctx, ln)
	if cerr := node.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		fmt.Fprintf(o.stdout, "ohya: stopped serving devices\n")
	}
	return err
}
