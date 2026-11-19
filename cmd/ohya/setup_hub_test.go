package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/transport/address"
)

func hubJoinFixture() setup.HubJoin {
	return setup.HubJoin{
		Code:          "ohya1.invitation.part",
		ExpiresAt:     "2026-11-16T12:00:00Z",
		DeviceCommand: deviceJoinCommand,
		RelayRegion:   "1 ohyatest",
		NodeKey:       "nodekey:abcdef",
	}
}

func TestHubProbeFailuresNameCauseAndNextStep(t *testing.T) {
	relay := hubProbeFailure(fmt.Errorf("%w: 127.0.0.1 needs outbound HTTPS (TCP 443): timeout", address.ErrRelayUnreachable))
	if !strings.Contains(relay.Error(), "outbound HTTPS (TCP 443)") || !strings.Contains(relay.Error(), "rerun ohya setup --hub") {
		t.Fatalf("relay failure = %v", relay)
	}
	hub := hubProbeFailure(fmt.Errorf("%w: dial timed out", address.ErrHubUnreachable))
	if !strings.Contains(hub.Error(), "ohya serve") || !strings.Contains(hub.Error(), "rerun ohya setup --hub") {
		t.Fatalf("hub failure = %v", hub)
	}
	other := errors.New("something else")
	if hubProbeFailure(other) != other {
		t.Fatal("unknown probe errors must pass through unchanged")
	}
}

func TestVersionMismatchFailureNamesVersionsAndUpgrade(t *testing.T) {
	frameErr := fmt.Errorf("%w: reply carries version 2, want 1", hubsync.ErrVersion)
	if !isVersionMismatch(frameErr) {
		t.Fatal("ErrVersion not recognized")
	}
	wireErr := &hubsync.Error{Code: hubsync.CodeVersion, Message: "frame carries version 1, want 2"}
	if !isVersionMismatch(wireErr) {
		t.Fatal("CodeVersion error not recognized")
	}
	if isVersionMismatch(errors.New("plain disconnect")) {
		t.Fatal("unrelated error misclassified")
	}
	failure := versionMismatchFailure(frameErr)
	for _, want := range []string{"this build speaks 1", "version 2", "want 1", "curl -fsSL", "install.sh | sh", "rerun the join"} {
		if !strings.Contains(failure.Error(), want) {
			t.Fatalf("failure %q missing %q", failure, want)
		}
	}
}

func TestPrintHubJoinMarksTheCodeSecret(t *testing.T) {
	var out strings.Builder
	printHubJoin(&out, hubJoinFixture())
	text := out.String()
	for _, want := range []string{"secret", "ohya1.invitation.part", "expires 2026-11-16T12:00:00Z", "curl -fsSL", "--join"} {
		if !strings.Contains(text, want) {
			t.Fatalf("hub block %q missing %q", text, want)
		}
	}
}

func TestCheckHubIdentityReportsMissingIdentity(t *testing.T) {
	f := checkHubIdentity(t.TempDir())
	if f.Severity != sevWarn || !strings.Contains(f.Detail, "no hub identity") || !strings.Contains(f.Remedy, "ohya setup --hub") {
		t.Fatalf("finding = %+v", f)
	}
}

func TestProbeHubWithRetryWaitsForTheServiceToAnswer(t *testing.T) {
	attempts := 0
	probe := func(context.Context, string) (address.HubInfo, error) {
		attempts++
		if attempts < 3 {
			return address.HubInfo{}, fmt.Errorf("%w: no answer yet", address.ErrHubUnreachable)
		}
		return address.HubInfo{}, nil
	}
	if err := probeHubWithRetry(context.Background(), "", 10*time.Second, time.Millisecond, probe); err != nil {
		t.Fatalf("retrying probe = %v, want success on the third attempt", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestProbeHubWithRetryBoundsTheWaitPerFailureClass(t *testing.T) {
	hubDown := func(context.Context, string) (address.HubInfo, error) {
		return address.HubInfo{}, fmt.Errorf("%w: no answer", address.ErrHubUnreachable)
	}
	err := probeHubWithRetry(context.Background(), "", 5*time.Millisecond, time.Millisecond, hubDown)
	if !strings.Contains(err.Error(), "ohya serve") || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("persistent hub failure = %v", err)
	}

	attempts := 0
	relay := func(context.Context, string) (address.HubInfo, error) {
		attempts++
		return address.HubInfo{}, fmt.Errorf("%w: 10.0.0.1 needs outbound HTTPS (TCP 443): refused", address.ErrRelayUnreachable)
	}
	err = probeHubWithRetry(context.Background(), "", 10*time.Second, time.Millisecond, relay)
	if !strings.Contains(err.Error(), "outbound HTTPS (TCP 443)") || attempts != 1 {
		t.Fatalf("relay failure = %v after %d attempts; want one attempt, no retry", err, attempts)
	}

	plain := errors.New("state directory unreadable")
	if err := probeHubWithRetry(context.Background(), "", 10*time.Second, time.Millisecond,
		func(context.Context, string) (address.HubInfo, error) { return address.HubInfo{}, plain }); err != plain {
		t.Fatalf("unknown probe error = %v; want it unchanged", err)
	}
}
