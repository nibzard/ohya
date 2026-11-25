package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/address"
	"tailscale.com/tailcfg"
)

var serveSpec = commandSpec{
	name:      "ohya serve",
	synopsis:  "run the hub for devices: transport, sync, remote tools",
	usageLine: "ohya serve [flags]",
	description: "Runs the hub side of device sync until the process receives SIGINT or SIGTERM. " +
		"SIGINT or SIGTERM exits 0 whenever it arrives — during startup, while the relay listener or the database opens, or while serving. " +
		"The hub identity — node key, pre-shared key, relay region — is created on the first start and saved owner-only under tailcat-hub/ in the state directory, so its address never changes across restarts. " +
		"The sync endpoint listens on port 9027 inside the Tailcat network stack and opens no port on the host; devices reach it through a DERP relay and a direct path when one exists. " +
		"Devices join with ohya join and one code from ohya devices invite.",
	examples: []string{"ohya serve", "ohya serve --db /srv/ohya/hub.db --state /srv/ohya"},
}

type serveFlagSet struct {
	c       *commandFlagSet
	db      *string
	state   *string
	observe *string
}

func newServeFlagSet(home string) *serveFlagSet {
	c := newCommandFlags(serveSpec)
	s := &serveFlagSet{c: c}
	s.db = c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	s.state = c.fs.String("state", "",
		"state `directory` for the device identity, registry, and Tailcat hub identity; defaults to the database directory")
	s.observe = c.fs.String("observe", "on",
		"publish local snapshots and metrics under the state directory's observe/ tree: on or off")
	return s
}

func runServeCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	s := newServeFlagSet(home)
	if err := s.c.parse(args, stdout); err != nil {
		return err
	}
	if s.c.fs.NArg() > 0 {
		return s.c.usageErrorf("unexpected argument %q", s.c.fs.Arg(0))
	}
	observeOn, err := parseObserveFlag(*s.observe)
	if err != nil {
		return s.c.usageErrorf("%s", err.Error())
	}
	stateDir := *s.state
	if stateDir == "" {
		stateDir = filepath.Dir(*s.db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, serveOptions{
		dbPath:   *s.db,
		stateDir: stateDir,
		stdout:   stdout,
		observe:  observeOn,
	})
}

type serveOptions struct {
	dbPath   string
	stateDir string
	stdout   io.Writer
	observe  bool

	region *tailcfg.DERPRegion
}

func runServe(ctx context.Context, o serveOptions) error {
	if err := os.MkdirAll(filepath.Dir(o.dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	node, err := address.NewHub(address.HubOptions{
		StateDir: o.stateDir,
		Region:   o.region,
	})
	if err != nil {
		return err
	}
	ln, err := node.Listen(ctx)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return err
	}

	telem := startTelemetry(o.stateDir, o.observe, sqliteSampleFiles(o.dbPath), observe.RoleServe)
	defer telem.close()
	hub, err := store.Open(ctx, o.dbPath, store.WithObserver(telem.observer()), store.WaitForLock(reportLockWait(o.stdout)))
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
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

	readers, err := hub.Reader()
	if err != nil {
		return err
	}
	defer readers.Close()
	engine := mcpserver.NewEngine(readers, hub.Writer(), mcpserver.EngineOptions{WriterID: deviceID, Usage: telem.usageSink(), Observer: telem.observer(), Events: telem.eventsSink()})
	auth := newPathRecorder(&registry.Authenticator{DB: reg}, node, reg, func(format string, args ...any) {
		fmt.Fprintf(o.stdout, "ohya: "+format+"\n", args...)
	})
	endpoint, err := hubsync.NewEndpoint(hub, auth, hubsync.Options{
		Remote:      engine,
		Diagnostics: hubDiagnosticsProvider{dbPath: o.dbPath, deviceID: deviceID, telem: telem},
		Observer:    telem.observer(),
	})
	if err != nil {
		return err
	}

	region := node.Region()
	fmt.Fprintf(o.stdout, "ohya: serving devices on the tailcat transport (relay region %d %s, node key %s)\n",
		region.RegionID, region.RegionCode, node.PublicKey())
	fmt.Fprintf(o.stdout, "ohya: invite a device: ohya devices invite --state %s\n", o.stateDir)

	err = endpoint.Serve(ctx, ln)
	_ = ln.Close()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = node.Drain(drainCtx)
	drainCancel()
	if cerr := node.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		fmt.Fprintf(o.stdout, "ohya: stopped serving devices\n")
	}
	return err
}

type hubDiagnosticsProvider struct {
	dbPath   string
	deviceID string
	telem    *telemetry
}

func (p hubDiagnosticsProvider) RemoteStatus(_ context.Context) (hubsync.RemoteStatus, error) {
	nowMS := time.Now().UnixMilli()
	id := statusCLIIdentity()
	out := hubsync.RemoteStatus{
		FormatVersion: hubsync.DiagnosticsFormatVersion,
		Hub: hubsync.RemoteIdentity{
			DeviceID: p.deviceID,
			Version:  id.Version,
			Revision: id.Revision,
			Dirty:    id.Dirty,
			Go:       id.Go,
			OS:       id.OS,
			Arch:     id.Arch,
			Schema:   id.Schema,
			Parsers:  id.Parsers,
		},
		GeneratedWallMS: nowMS,
		Roles:           []string{string(observe.RoleServe)},
		Limits: hubsync.RemoteLimits{
			MaxConns:       hubsync.DefaultMaxConns,
			MaxBatchBytes:  hubsync.MaxBatchBytes,
			MaxRecordBytes: hubsync.MaxRecordBytes,
			QueryTimeoutMS: mcpserver.DefaultDeadline.Milliseconds(),
			QueryMaxRows:   mcpserver.DefaultMaxRows,
			QueryMaxBytes:  mcpserver.DefaultMaxOutputBytes,
		},
	}
	if p.telem != nil && p.telem.recorder != nil {
		snap := p.telem.recorder.Snapshot()
		uptime := snap.UptimeMS
		sampled := snap.SampledWallMS
		out.UptimeMS = &uptime
		out.SampledWallMS = &sampled
		if len(snap.Instance.Roles) > 0 {
			out.Roles = append([]string(nil), snap.Instance.Roles...)
		}
		if len(snap.Resources) > 0 {
			out.Resources = snap.Resources
		}
		files := map[string]int64{}
		for label, value := range snap.Gauges {
			if after, ok := cutPrefix(label, "sqlite_file_bytes{file="); ok {
				files[trimSuffix(after, "}")] = int64(value)
			}
		}
		if len(files) > 0 {
			out.StorageFiles = files
			out.StorageObservedWallMS = sampled
		}
	}
	if len(out.StorageFiles) == 0 && p.dbPath != "" {
		files := map[string]int64{}
		for label, suffix := range map[string]string{"db": "", "wal": "-wal", "shm": "-shm"} {
			if info, err := os.Stat(p.dbPath + suffix); err == nil && info.Mode().IsRegular() {
				files[label] = info.Size()
			}
		}
		if len(files) > 0 {
			out.StorageFiles = files
			out.StorageObservedWallMS = nowMS
		}
	}
	return out, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}
