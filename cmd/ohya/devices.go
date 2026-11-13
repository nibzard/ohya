// The devices subcommand: the hub operator's management surface for pairing.
// It issues one-time expiring invitations, lists the device registry, and
// revokes devices. The registry lives in its own protected database next to
// the hub database, never inside the queryable transcript tables.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nibzard/ohya/internal/registry"
)

const devicesUsage = `ohya devices — manage paired devices

Usage:

  ohya devices invite [--ttl 15m]   Issue a one-time pairing invitation
  ohya devices list                 List devices and invitations
  ohya devices revoke <device-id>   Revoke a device, final for its identity
`

// runDevicesCommand dispatches the devices subcommands.
func runDevicesCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, devicesUsage)
		return errors.New("missing devices subcommand")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "invite":
		return runDevicesInvite(rest, stdout, stderr)
	case "list":
		return runDevicesList(rest, stdout, stderr)
	case "revoke":
		return runDevicesRevoke(rest, stdout, stderr)
	default:
		fmt.Fprint(stderr, devicesUsage)
		return fmt.Errorf("unknown devices subcommand %q", cmd)
	}
}

// registryFlags holds the flags every devices subcommand shares. The registry
// sits in the state directory, like the device identity and checkpoints.
// Callers add their own flags, then Parse.
type registryFlags struct {
	fs    *flag.FlagSet
	state *string
}

func newRegistryFlags(name string, stderr io.Writer) (*registryFlags, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	f := &registryFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	f.fs.SetOutput(stderr)
	f.state = f.fs.String("state", filepath.Join(home, ".ohya"),
		"state `directory` holding the device registry")
	return f, nil
}

// openRegistry opens the registry database in the state directory.
func (f *registryFlags) openRegistry() (*registry.DB, error) {
	return registry.Open(filepath.Join(*f.state, registry.FileName))
}

// runDevicesInvite issues one invitation and prints its code on the first
// stdout line, so it can be piped to the joining device.
func runDevicesInvite(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags("ohya devices invite", stderr)
	if err != nil {
		return err
	}
	ttl := flags.fs.Duration("ttl", registry.DefaultInvitationTTL,
		"how long the invitation stays consumable")
	if err := flags.fs.Parse(args); err != nil {
		return err
	}
	if flags.fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.fs.Arg(0))
	}

	reg, err := flags.openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()
	inv, err := reg.Issue(context.Background(), *ttl)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, inv.Secret)
	fmt.Fprintf(stderr, "expires %s (one device may use it once)\n", msTime(inv.ExpiresAtMS))
	return nil
}

// runDevicesList prints every paired device and every invitation.
func runDevicesList(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags("ohya devices list", stderr)
	if err != nil {
		return err
	}
	if err := flags.fs.Parse(args); err != nil {
		return err
	}
	if flags.fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.fs.Arg(0))
	}
	reg, err := flags.openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()
	ctx := context.Background()

	devices, err := reg.List(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "devices:")
	if len(devices) == 0 {
		fmt.Fprintln(stdout, "  (none paired)")
	}
	for _, d := range devices {
		line := fmt.Sprintf("  %s  %s  paired %s",
			d.DeviceID, d.State, msTime(d.PairedAtMS))
		if d.RevokedAtMS != 0 {
			line += fmt.Sprintf(", revoked %s", msTime(d.RevokedAtMS))
		}
		fmt.Fprintln(stdout, line)
	}

	invitations, err := reg.ListInvitations(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "invitations:")
	if len(invitations) == 0 {
		fmt.Fprintln(stdout, "  (none issued)")
	}
	for _, inv := range invitations {
		switch {
		case inv.State == "used":
			fmt.Fprintf(stdout, "  %s  used by %s\n", inv.ID, inv.UsedBy)
		case inv.Expired:
			fmt.Fprintf(stdout, "  %s  expired\n", inv.ID)
		default:
			fmt.Fprintf(stdout, "  %s  open until %s\n", inv.ID, msTime(inv.ExpiresAtMS))
		}
	}
	return nil
}

// runDevicesRevoke revokes one device. Revocation is final for the device
// identity; its connections close and re-admission needs a new identity.
func runDevicesRevoke(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags("ohya devices revoke", stderr)
	if err != nil {
		return err
	}
	if err := flags.fs.Parse(args); err != nil {
		return err
	}
	if flags.fs.NArg() != 1 {
		return errors.New("revoke needs exactly one device ID; see ohya devices list")
	}
	deviceID := flags.fs.Arg(0)
	reg, err := flags.openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()
	if err := reg.Revoke(context.Background(), deviceID); err != nil {
		return err
	}
	fmt.Fprintln(stdout, deviceID, "revoked")
	return nil
}

// msTime renders a Unix-millisecond timestamp as UTC RFC 3339.
func msTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
