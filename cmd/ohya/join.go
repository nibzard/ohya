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
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/transport"
	"github.com/nibzard/ohya/internal/transport/address"
	"github.com/nibzard/ohya/internal/uploader"
	"github.com/nibzard/ohya/internal/watch"
)

type joinOptions struct {
	pairOnly   bool
	allowEmpty bool
	observe    bool
	stateDir   string
	code       string
	roots      []agentRoot
	excludes   excludeGlobs
	interval   time.Duration
	stderr     io.Writer
}

var joinSpec = commandSpec{
	name:      "ohya join",
	synopsis:  "pair a device with a hub and upload its agent sessions",
	usageLine: "ohya join [code] [flags]",
	description: "Pairs this device with a hub using one join code — printed by ohya devices invite on the hub — then uploads every watched agent source: batches of original records, acknowledged durably, retried after reconnects, catching up offline from the retained files. " +
		"SIGINT or SIGTERM stops join cleanly at any point — while pairing, while the hub verifies a credential, or while uploading — and the process exits 0. " +
		"A device that already holds a credential sends its hello to the hub address in the code first: when the hub accepts it, the device keeps its identity, credential, and checkpoints, and the invitation stays unused; when the hub rejects it, join fails with the reason and never pairs as a new device. " +
		"The join state directory is deliberately its own (default ~/.ohya/join): its checkpoints hold this hub's acknowledgments and must never mix with local ingestion's.\n" +
		"Repeatable --exclude GLOB flags skip matching session files at discovery, exactly as on ohya ingest: an excluded path is never registered, read, or sent, and ohya status reports the per-agent skip counts.",
	args:     "<code>  join code from ohya devices invite; omit it to resume an already-paired device",
	examples: []string{"ohya join <code>", "ohya join --state ~/.ohya/join <code>"},
}

func runJoinCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(joinSpec)
	state := c.fs.String("state", filepath.Join(home, ".ohya", "join"),
		"join state `directory`: device identity, pairing credential, hub address, and this hub's upload checkpoints")
	codeEnv := c.fs.String("join-code-env", "",
		"environment variable holding the join code (avoids exposing it in process listings)")
	rootFlags := newAgentRootFlags(c.fs, home)
	excludes := newExcludeFlag(c.fs)
	allowEmpty := c.fs.Bool("allow-empty", false, "wait for session roots that do not yet exist")
	interval := c.fs.Duration("interval", 2*time.Second,
		"polling `interval` between upload passes")
	observeFlag := c.fs.String("observe", "on",
		"publish local snapshots and metrics under the join state directory's observe/ tree: on or off")
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
	if *codeEnv != "" {
		if code != "" {
			return c.usageErrorf("pass either a positional join code or --join-code-env, not both")
		}
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(*codeEnv) {
			return c.usageErrorf("invalid join code environment variable name")
		}
		code = strings.TrimSpace(os.Getenv(*codeEnv))
		if code == "" {
			return c.usageErrorf("join code environment variable is empty")
		}
		if _, _, err := parseJoinCode(code); err != nil {
			return c.usageErrorf("join code environment variable does not hold one: %v", err)
		}
		if err := os.Unsetenv(*codeEnv); err != nil {
			return err
		}
	}
	if *interval <= 0 {
		return c.usageErrorf("--interval must be positive")
	}
	observeOn, err := parseObserveFlag(*observeFlag)
	if err != nil {
		return c.usageErrorf("%s", err.Error())
	}
	roots := rootFlags.roots(c.fs)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJoin(ctx, joinOptions{
		allowEmpty: *allowEmpty,
		observe:    observeOn,
		stateDir:   *state,
		code:       code,
		roots:      roots,
		excludes:   *excludes,
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
	invitation, codeAddr := "", ""
	if o.code != "" {
		invitation, codeAddr, err = parseJoinCode(o.code)
		if err != nil {
			return err
		}
	}
	node := address.NewDevice(address.DeviceOptions{})
	defer node.Close()

	hubAddr := ""
	cred, err := device.LoadCredential(o.stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if o.code == "" {
			return errors.New("this device is not paired yet; pass the join code printed by ohya devices invite")
		}
		hubAddr = codeAddr
		cred, err = pair(ctx, node, hubAddr, invitation, deviceID)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		if err := device.SaveCredential(o.stateDir, cred); err != nil {
			return err
		}
		if err := device.SaveHubAddress(o.stateDir, hubAddr); err != nil {
			return err
		}
		fmt.Fprintf(o.stderr, "ohya: paired as device %s\n", cred.DeviceID)
	case err != nil:
		return err
	default:
		hubAddr, err = rejoin(ctx, node, o.stateDir, cred, o.code, codeAddr, o.stderr)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
	}

	reportJoinTransport(ctx, node, hubAddr, o.stderr)
	if o.pairOnly {
		return nil
	}
	if len(o.excludes) > 0 {
		fmt.Fprintf(o.stderr, "ohya: excluding session files matching: %s\n", strings.Join(o.excludes, ", "))
	}
	reportJoinBacklog(o.stateDir, o.roots, o.excludes, o.stderr)

	telem := startTelemetry(o.stateDir, o.observe, nil, observe.RoleJoin)
	defer telem.close()
	dial := func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, hubAddr) }
	errs := make(chan error, len(o.roots))
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	for _, root := range o.roots {
		w, err := watch.New(o.stateDir, root.agent, watch.Options{Observer: telem.observer()})
		if err != nil {
			return err
		}
		dirs := root.dirs
		agent := root.agent
		observer := telem.observer()
		discover, excluded := joinDiscover(dirs, agent, o.excludes, o.allowEmpty, observer, o.stderr)
		up, err := uploader.New(w, cred, dial, uploader.Options{
			Agent:    root.agent,
			Interval: o.interval,
			Observer: telem.observer(),
			Events:   telem.eventsSink(),
			Log:      o.stderr,
			Excluded: excluded,
		})
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- up.Run(runCtx, discover)
		}()
	}
	select {
	case <-ctx.Done():
		cancel()
		wg.Wait()
		drainDevice(node)
		return nil
	case err := <-errs:
		cancel()
		wg.Wait()
		drainDevice(node)
		return err
	}
}

func rejoin(ctx context.Context, node transport.Device, stateDir string, cred device.Credential, code, codeAddr string, stderr io.Writer) (string, error) {
	if code == "" {
		hubAddr, err := device.LoadHubAddress(stateDir)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("device %s is paired but holds no hub address, as after an upgrade from a release that paired without a join code; on the hub, run ohya devices invite, then on this device run: %s", cred.DeviceID, deviceJoinCommand)
		}
		if err != nil {
			return "", err
		}
		return hubAddr, nil
	}
	if err := verifyCredential(ctx, node, codeAddr, cred); err != nil {
		return "", err
	}
	if err := device.SaveHubAddress(stateDir, codeAddr); err != nil {
		return "", err
	}
	fmt.Fprintf(stderr, "ohya: hub accepted device %s; kept identity and checkpoints, the invitation stays unused\n", cred.DeviceID)
	return codeAddr, nil
}

func verifyCredential(ctx context.Context, node transport.Device, hubAddr string, cred device.Credential) error {
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	session := hubsync.NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, hubAddr) },
		cred.Secret, hubsync.RemoteSessionOptions{})
	defer session.Close()
	if _, err := session.Query(probeCtx, "SELECT 1"); err != nil {
		if errors.Is(err, hubsync.ErrRemoteUnauthorized) {
			return fmt.Errorf("the hub refused this device's credential: %w; the device identity was revoked, or the code names a different hub", err)
		}
		if isVersionMismatch(err) {
			return versionMismatchFailure(err)
		}
		return fmt.Errorf("cannot reach the hub at the address in this code: %w; check that ohya serve runs on the hub and this device can reach a relay (outbound HTTPS, TCP 443)", err)
	}
	return nil
}

func pair(ctx context.Context, node transport.Device, hubAddr, invitation, deviceID string) (device.Credential, error) {
	conn, err := node.Dial(ctx, hubAddr)
	if err != nil {
		return device.Credential{}, fmt.Errorf("join: reach the hub: %w; check that ohya serve runs on the hub and this device can reach a relay (outbound HTTPS, TCP 443)", err)
	}
	defer conn.Close()
	secret, err := hubsync.Pair(conn, invitation, deviceID)
	if err != nil {
		var perr *hubsync.Error
		if errors.As(err, &perr) && perr.Code == hubsync.CodeAuth {
			return device.Credential{}, fmt.Errorf("join: pair with hub: %w; when the invitation expired or was used, ask the hub for a fresh code from ohya devices invite", err)
		}
		if isVersionMismatch(err) {
			return device.Credential{}, versionMismatchFailure(err)
		}
		return device.Credential{}, fmt.Errorf("join: pair with hub: %w", err)
	}
	return device.Credential{DeviceID: deviceID, Secret: secret}, nil
}

func drainDevice(node *address.Device) {
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = node.Drain(drainCtx)
	cancel()
}

func reportJoinTransport(ctx context.Context, node *address.Device, hubAddr string, stderr io.Writer) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	reportCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report, err := node.PathInfo(reportCtx, hubAddr)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "ohya: hub path unknown (the transport library reported no measurement: %v); sync protocol %d\n", err, hubsync.ProtocolVersion)
	case report.Path == address.PathRelay:
		fmt.Fprintf(stderr, "ohya: hub answers through a relay path (region %s), round trip %.1f ms; sync protocol %d\n",
			relayRegionText(report.RegionID, report.RegionCode), report.RoundTripMS, hubsync.ProtocolVersion)
	default:
		fmt.Fprintf(stderr, "ohya: hub answers through a %s path, round trip %.1f ms; sync protocol %d\n",
			report.Path, report.RoundTripMS, hubsync.ProtocolVersion)
	}
}

func joinDiscover(dirs []agentDir, agent string, excludes excludeGlobs, allowEmpty bool, observer observe.Observer, stderr io.Writer) (func() ([]string, error), func(string) bool) {
	excludedPaths := map[string]bool{}
	lastExcluded := -1
	discover := func() ([]string, error) {
		candidates, excluded, err := discoverAgentSessionFiles(agent, dirs, excludes, allowEmpty)
		if err != nil {
			return nil, err
		}
		if len(excludes) > 0 {
			excludedPaths = excludedPathSet(excluded)
			observer.Set("discovery_excluded", float64(len(excluded)), agent)
			if len(excluded) != lastExcluded {
				lastExcluded = len(excluded)
				fmt.Fprintf(stderr, "ohya: %s: %d session files excluded by --exclude\n", agent, len(excluded))
			}
		}
		return candidates, nil
	}
	return discover, func(path string) bool { return excludedPaths[path] }
}

func reportJoinBacklog(stateDir string, roots []agentRoot, excludes excludeGlobs, stderr io.Writer) {
	var pending int64
	for _, root := range roots {
		candidates, _, err := discoverAgentSessionFiles(root.agent, root.dirs, excludes, false)
		if err != nil {
			continue
		}
		snaps, err := watch.InspectSources(stateDir, root.agent)
		if err != nil {
			fmt.Fprintf(stderr, "ohya: upload backlog for %s unknown: %v\n", root.agent, err)
			continue
		}
		byPath := make(map[string]int64, len(snaps))
		for _, s := range snaps {
			byPath[s.Path] = s.CursorBytes
		}
		dirCache := make(dirEvalCache)
		for _, path := range candidates {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			var acknowledged int64
			for _, form := range dirCache.regularPathForms(path) {
				if cursor, ok := byPath[form]; ok {
					acknowledged = cursor
					break
				}
			}
			if delta := info.Size() - acknowledged; delta > 0 {
				pending += delta
			}
		}
	}
	fmt.Fprintf(stderr, "ohya: upload backlog: %d bytes\n", pending)
}
