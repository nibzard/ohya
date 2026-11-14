// The join subcommand: the device side of sync. One join code — printed by
// `ohya devices invite`, carrying the T018 pairing invitation and the
// single-use coordinator preauth key minted with it — enrolls this device's
// transport node into the hub's tailnet and pairs its identity: the hub
// mints and delivers the credential once, and the device keeps it in its
// join state directory. Afterwards join uploads every watched agent source
// to the hub through internal/uploader: batches of original records,
// acknowledged durably, retried after reconnects, catching up offline from
// the retained files.
//
// The join state directory is deliberately its own (default ~/.ohya/join):
// its checkpoint files hold this hub's acknowledgments, which must never mix
// with the acknowledgments of local ingestion on a machine that does both
// (T019).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
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

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/transport/tailcat"
	"github.com/nibzard/ohya/internal/uploader"
	"github.com/nibzard/ohya/internal/watch"
)

// joinOptions configures one join run.
type joinOptions struct {
	stateDir   string
	controlURL string
	hubAddr    string
	code       string // join code; empty resumes an already-paired device
	roots      []agentRoot
	interval   time.Duration
	stderr     io.Writer
}

// runJoinCommand parses the join flags and runs the device side until the
// process receives SIGINT or SIGTERM.
func runJoinCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	fs := flag.NewFlagSet("ohya join", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", filepath.Join(home, ".ohya", "join"),
		"join state `directory`: device identity, pairing credential, transport node, and this hub's upload checkpoints")
	controlURL := fs.String("control", control.DefaultURL(),
		"coordination `URL` of the hub's tailnet")
	hubAddr := fs.String("hub-addr", tailcat.DefaultHubAddr(),
		"hub `address` inside the tailnet")
	claudeRoot := fs.String("claude-root", filepath.Join(home, ".claude", "projects"),
		"Claude Code projects `directory` to watch; empty disables Claude Code")
	codexRoot := fs.String("codex-root", defaultCodexRoot(home),
		"Codex sessions `directory` to watch ($CODEX_HOME/sessions when set); empty disables Codex")
	piRoot := fs.String("pi-root", defaultPiRoot(home),
		"Pi agent sessions `directory` to watch ($PI_CODING_AGENT_SESSION_DIR or $PI_CODING_AGENT_DIR/sessions when set); empty disables Pi")
	interval := fs.Duration("interval", 2*time.Second,
		"polling `interval` between upload passes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	code := ""
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(1))
	}
	if fs.NArg() == 1 {
		code = fs.Arg(0)
	}
	if *interval <= 0 {
		return errors.New("-interval must be positive")
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var roots []agentRoot
	for _, r := range []agentRoot{
		{agent: claudecode.AgentName, dir: *claudeRoot, flag: "claude-root"},
		{agent: codex.AgentName, dir: *codexRoot, flag: "codex-root"},
		{agent: pi.AgentName, dir: *piRoot, flag: "pi-root"},
	} {
		if r.dir == "" {
			continue
		}
		r.optional = !explicit[r.flag]
		roots = append(roots, r)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJoin(ctx, joinOptions{
		stateDir:   *state,
		controlURL: *controlURL,
		hubAddr:    *hubAddr,
		code:       code,
		roots:      roots,
		interval:   *interval,
		stderr:     stderr,
	})
}

// runJoin pairs the device when needed, then uploads every watched agent
// until ctx is done. It returns only when retrying cannot help: an unusable
// identity or credential, a refused pairing, or a watcher state needing
// recovery.
func runJoin(ctx context.Context, o joinOptions) error {
	if len(o.roots) == 0 {
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
	node, err := tailcat.NewDevice(tailcat.Options{
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

	dial := func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, o.hubAddr) }
	errs := make(chan error, len(o.roots))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, root := range o.roots {
		w, err := watch.New(o.stateDir, root.agent, watch.Options{})
		if err != nil {
			return err // includes watch.ErrStateLost: recovery, not a silent restart
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
				if optional && errors.Is(err, fs.ErrNotExist) {
					return nil, nil // a default directory of an agent not installed
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

// pair enrolls the transport node (the dial brings it up, consuming the
// single-use preauth key of the join code on a first enrollment) and
// consumes the pairing invitation on one connection: the hub mints the
// credential and delivers it exactly once, then the connection closes.
func pair(ctx context.Context, node *tailcat.Node, hubAddr, invitation, deviceID string) (device.Credential, error) {
	conn, err := node.Dial(ctx, hubAddr)
	if err != nil {
		return device.Credential{}, fmt.Errorf("join: reach the hub at %s: %w", hubAddr, err)
	}
	defer conn.Close()
	if err := hubsync.WritePairRequest(conn, invitation, deviceID); err != nil {
		return device.Credential{}, fmt.Errorf("join: send pairing request: %w", err)
	}
	reply, perr, err := hubsync.ReadCredentialReply(bufio.NewReader(conn))
	if err != nil {
		return device.Credential{}, fmt.Errorf("join: read pairing reply: %w", err)
	}
	if perr != nil {
		return device.Credential{}, fmt.Errorf("join: hub refused the pairing: %w", perr)
	}
	return device.Credential{DeviceID: reply.DeviceID, Secret: reply.Credential}, nil
}

// deviceHostname derives the device's stable tailnet hostname from its
// identity: DNS-safe, unique per device identity, stable across runs.
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
