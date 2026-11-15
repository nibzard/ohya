package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/nibzard/ohya/internal/transport"
	"github.com/nibzard/ohya/internal/transport/tailcat"
	"github.com/nibzard/ohya/internal/uploader"
	"github.com/nibzard/ohya/internal/watch"
)

type joinOptions struct {
	pairOnly   bool
	allowEmpty bool
	stateDir   string
	controlURL string
	hubAddr    string
	code       string
	roots      []agentRoot
	interval   time.Duration
	stderr     io.Writer
}

var joinSpec = commandSpec{
	name:      "ohya join",
	synopsis:  "pair a device with a hub and upload its agent sessions",
	usageLine: "ohya join [code] [flags]",
	description: "Pairs this device with a hub using one join code — printed by ohya devices invite on the hub — then uploads every watched agent source: batches of original records, acknowledged durably, retried after reconnects, catching up offline from the retained files. " +
		"The join state directory is deliberately its own (default ~/.ohya/join): its checkpoints hold this hub's acknowledgments and must never mix with local ingestion's.",
	argsTitle: "Arguments",
	args:      "<code>  join code from ohya devices invite; omit it to resume an already-paired device",
	examples:  []string{"ohya join <code>", "ohya join --state ~/.ohya/join <code>"},
}

func runJoinCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(joinSpec)
	state := c.fs.String("state", filepath.Join(home, ".ohya", "join"),
		"join state `directory`: device identity, pairing credential, transport node, and this hub's upload checkpoints")
	controlURL := c.fs.String("control", control.DefaultURL(),
		"coordination `URL` of the hub's tailnet")
	hubAddr := c.fs.String("hub-addr", tailcat.DefaultHubAddr(),
		"hub `address` inside the tailnet")
	rootFlags := newAgentRootFlags(c.fs, home)
	allowEmpty := c.fs.Bool("allow-empty", false, "wait for session roots that do not yet exist")
	interval := c.fs.Duration("interval", 2*time.Second,
		"polling `interval` between upload passes")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	code := ""
	if c.fs.NArg() > 1 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(1))
	}
	if c.fs.NArg() == 1 {
		code = c.fs.Arg(0)
	}
	if *interval <= 0 {
		return c.usageErrorf("--interval must be positive")
	}
	roots := rootFlags.roots(c.fs)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJoin(ctx, joinOptions{
		allowEmpty: *allowEmpty,
		stateDir:   *state,
		controlURL: *controlURL,
		hubAddr:    *hubAddr,
		code:       code,
		roots:      roots,
		interval:   *interval,
		stderr:     stderr,
	})
}

func runJoin(ctx context.Context, o joinOptions) error {
	if len(o.roots) == 0 && !o.pairOnly {
		return errors.New("no agent session directory to watch")
	}
	if err := os.MkdirAll(o.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	deviceID, err := device.LoadOrCreate(o.stateDir)
	if err != nil {
		return err
	}
	invitation, authKey := "", ""
	if o.code != "" {
		invitation, authKey, err = control.SplitJoinCode(o.code)
		if err != nil {
			return err
		}
	}
	node, err := tailcat.NewSharedDevice(tailcat.Options{
		Hostname:   deviceHostname(deviceID),
		AuthKey:    authKey,
		ControlURL: o.controlURL,
		StateDir:   filepath.Join(o.stateDir, "tailcat"),
	})
	if err != nil {
		return err
	}
	defer node.Close()

	cred, err := device.LoadCredential(o.stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if o.code == "" {
			return errors.New("this device is not paired yet; pass the join code printed by ohya devices invite")
		}
		cred, err = pair(ctx, node, o.hubAddr, invitation, deviceID)
		if err != nil {
			return err
		}
		if err := device.SaveCredential(o.stateDir, cred); err != nil {
			return err
		}
		fmt.Fprintf(o.stderr, "ohya: paired as device %s\n", cred.DeviceID)
	case err != nil:
		return err
	}

	if o.pairOnly {
		return nil
	}

	dial := func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, o.hubAddr) }
	errs := make(chan error, len(o.roots))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, root := range o.roots {
		w, err := watch.New(o.stateDir, root.agent, watch.Options{})
		if err != nil {
			return err
		}
		up, err := uploader.New(w, cred, dial, uploader.Options{
			Agent:    root.agent,
			Interval: o.interval,
			Log:      o.stderr,
		})
		if err != nil {
			return err
		}
		dir, optional := root.dir, root.optional
		go func() {
			errs <- up.Run(runCtx, func() ([]string, error) {
				candidates, err := discoverSessionFiles(dir)
				if (optional || o.allowEmpty) && errors.Is(err, fs.ErrNotExist) {
					return nil, nil
				}
				return candidates, err
			})
		}()
	}
	select {
	case <-ctx.Done():
		cancel()
		return nil
	case err := <-errs:
		cancel()
		return err
	}
}

func pair(ctx context.Context, node transport.Device, hubAddr, invitation, deviceID string) (device.Credential, error) {
	conn, err := node.Dial(ctx, hubAddr)
	if err != nil {
		return device.Credential{}, fmt.Errorf("join: reach the hub at %s: %w", hubAddr, err)
	}
	defer conn.Close()
	secret, err := hubsync.Pair(conn, invitation, deviceID)
	if err != nil {
		return device.Credential{}, fmt.Errorf("join: pair with hub: %w", err)
	}
	return device.Credential{DeviceID: deviceID, Secret: secret}, nil
}

func deviceHostname(deviceID string) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return -1
		}
	}, strings.ToLower(deviceID))
	if len(id) > 12 {
		id = id[:12]
	}
	if id == "" {
		id = "device"
	}
	return "dev-" + id
}
