package main

import (
	"context"
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

const hubAuthKeyTTL = 10 * time.Minute

var serveSpec = commandSpec{
	name:      "ohya serve",
	synopsis:  "run the hub for devices: coordination, sync, remote tools",
	usageLine: "ohya serve [flags]",
	description: "Runs the hub side of device sync until the process receives SIGINT or SIGTERM. " +
		"It provisions the self-hosted coordination plane in-process — own database, generated configuration, embedded DERP relay — enrolls the hub's own transport node, and serves the sync endpoint on the encrypted tailnet: registration, device batches, and the remote query and annotate tools through the same bounded engine as the stdio tools. " +
		"Devices join with ohya join and one code from ohya devices invite.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya serve", "ohya serve --control-url http://192.0.2.10:8080", "ohya serve --tls-cert cert.pem --tls-key key.pem --control-url https://192.0.2.10:9417"},
}

func runServeCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(serveSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	state := c.fs.String("state", "",
		"state `directory` for the device identity, registry, and coordination plane; defaults to the database directory")
	controlListen := c.fs.String("control-listen", control.DefaultListenAddr,
		"listen `address` of the coordination plane and its embedded DERP relay")
	controlURL := c.fs.String("control-url", "",
		"coordination `URL` devices reach the hub at; defaults to the scheme of --tls-cert/--tls-key plus --control-listen (pass it when the hub is reachable from other machines)")
	tlsCert := c.fs.String("tls-cert", "",
		"TLS certificate `path` for the coordination plane and its embedded DERP relay; requires --tls-key, makes the default --control-url https, and devices must trust the certificate's CA")
	tlsKey := c.fs.String("tls-key", "",
		"TLS key `path` for the coordination plane; requires --tls-cert")
	stunListen := c.fs.String("stun-listen", control.DefaultSTUNAddr,
		"UDP listen `address` of the coordination plane's STUN helper for devices behind NAT")
	controlLogLevel := c.fs.String("control-log-level", "warn",
		"coordination plane log `level`: warn, info or debug")
	tailnetPort := c.fs.Int("tailnet-port", tailcat.DefaultHubPort,
		"tailnet `port` the sync endpoint listens on inside the transport")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	url := *controlURL
	if url == "" {
		host, _, err := net.SplitHostPort(*controlListen)
		if err != nil {
			return fmt.Errorf("parse --control-listen: %w", err)
		}
		if host == "0.0.0.0" || host == "::" || strings.Contains(host, ":") {
			return c.usageErrorf("--control-listen binds every interface; pass --control-url with the address devices use")
		}
		scheme := "http"
		if *tlsCert != "" || *tlsKey != "" {
			scheme = "https"
		}
		url = scheme + "://" + *controlListen
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
		tlsCertPath:     *tlsCert,
		tlsKeyPath:      *tlsKey,
		tailnetPort:     *tailnetPort,
		stdout:          stdout,
	})
}

type serveOptions struct {
	dbPath          string
	stateDir        string
	controlListen   string
	controlURL      string
	stunListen      string
	controlLogLevel string
	tlsCertPath     string
	tlsKeyPath      string
	tailnetPort     int
	stdout          io.Writer
}

func runServe(ctx context.Context, o serveOptions) error {
	if err := os.MkdirAll(filepath.Dir(o.dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	ctl, err := control.Provision(control.Options{
		Dir:         filepath.Join(o.stateDir, control.DirName),
		ServerURL:   o.controlURL,
		ListenAddr:  o.controlListen,
		STUNAddr:    o.stunListen,
		BaseDomain:  tailcat.DefaultBaseDomain,
		LogLevel:    o.controlLogLevel,
		TLSCertPath: o.tlsCertPath,
		TLSKeyPath:  o.tlsKeyPath,
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

	err = endpoint.Serve(ctx, ln)
	if cerr := node.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		fmt.Fprintf(o.stdout, "ohya: stopped serving devices\n")
	}
	return err
}
