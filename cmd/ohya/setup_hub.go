package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/transport/address"
)

const hubIdentityWait = 60 * time.Second

const deviceJoinCommand = "curl -fsSL https://github.com/nibzard/ohya/releases/latest/download/install.sh | sh -s -- --join"

const upgradeCommand = "curl -fsSL https://github.com/nibzard/ohya/releases/latest/download/install.sh | sh"

func checkHubSetup(ctx context.Context, state string) error {
	if err := waitForHubAddress(ctx, state); err != nil {
		return err
	}
	_, err := address.ProbeHub(ctx, state)
	if err != nil {
		return hubProbeFailure(err)
	}
	return nil
}

func waitForHubAddress(ctx context.Context, state string) error {
	deadline := time.Now().Add(hubIdentityWait)
	for {
		_, err := address.LoadHubAddr(state)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, address.ErrNoIdentity):
			return err
		case time.Now().After(deadline):
			return fmt.Errorf("the ohya serve service created no hub identity within %s: check its logs (journalctl --user -u ohya-serve.service, or launchctl print gui/%d/com.nibzard.ohya.serve), then rerun ohya setup --hub",
				hubIdentityWait, os.Getuid())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func hubProbeFailure(err error) error {
	switch {
	case errors.Is(err, address.ErrRelayUnreachable):
		return fmt.Errorf("no relay is reachable: the hub needs outbound HTTPS (TCP 443) to its DERP relay; allow it, then rerun ohya setup --hub (%v)", err)
	case errors.Is(err, address.ErrHubUnreachable):
		return fmt.Errorf("the hub did not answer through its relay: check whether the ohya serve service runs (systemctl --user status ohya-serve.service, or launchctl print gui/%d/com.nibzard.ohya.serve), then rerun ohya setup --hub (%v)", os.Getuid(), err)
	default:
		return err
	}
}

func finishHubSetup(state string, ttl time.Duration) (*setup.HubJoin, error) {
	info, err := address.LoadHubInfo(state)
	if err != nil {
		return nil, err
	}
	code, expiresAtMS, err := issueJoinCode(state, ttl)
	if err != nil {
		return nil, err
	}
	region := fmt.Sprintf("%d %s", info.RegionID, info.RegionCode)
	if info.RegionName != "" {
		region += " " + info.RegionName
	}
	return &setup.HubJoin{
		Code:          code,
		ExpiresAt:     msTime(expiresAtMS),
		DeviceCommand: deviceJoinCommand,
		RelayRegion:   region,
		NodeKey:       info.NodeKey,
	}, nil
}

func printHubJoin(stdout io.Writer, hub setup.HubJoin) {
	fmt.Fprintf(stdout, "ohya: hub: relay region %s, node key %s\n", hub.RelayRegion, hub.NodeKey)
	fmt.Fprintf(stdout, "ohya: join code (secret, one use, expires %s):\n%s\n", hub.ExpiresAt, hub.Code)
	fmt.Fprintf(stdout, "ohya: on each device run: %s\n", hub.DeviceCommand)
}

func isVersionMismatch(err error) bool {
	var perr *hubsync.Error
	if errors.As(err, &perr) && perr.Code == hubsync.CodeVersion {
		return true
	}
	return errors.Is(err, hubsync.ErrVersion)
}

func versionMismatchFailure(err error) error {
	return fmt.Errorf("the hub and this device speak different sync protocol versions: this build speaks %d (%v); upgrade ohya on the older machine with: %s; then rerun the join",
		hubsync.ProtocolVersion, err, upgradeCommand)
}

func hubServiceArgs(state, db string) []string {
	return []string{"serve", "--db", db, "--state", state}
}
