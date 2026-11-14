// The devices subcommand: the hub operator's management surface for pairing.
// It issues one-time expiring invitations, lists the device registry, and
// revokes devices. The registry lives in its own protected database next to
// the hub database, never inside the queryable transcript tables.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/registry"
)

// devicesSpec is the devices dispatch usage template; the subcommands carry
// their own.
var devicesSpec = commandSpec{
	name:      "ohya devices",
	synopsis:  "manage device pairing and revocation",
	usageLine: "ohya devices <command> [flags]",
	description: "The hub operator's surface for pairing: issue one-time expiring invitations, list the device registry, and revoke devices. " +
		"The registry lives in its own protected database in the state directory, never inside the queryable transcript tables.",
	argsTitle: "Commands",
	args: `invite              issue a one-time join code
list                list devices and invitations
revoke <device-id>  revoke a device, final for its identity`,
	examples: []string{"ohya devices invite --ttl 15m", "ohya devices revoke <device-id>"},
}

// runDevicesCommand dispatches the devices subcommands.
func runDevicesCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		writeUsage(stdout, devicesSpec, nil)
		return nil
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
		return usageError{cmd: "ohya devices", msg: fmt.Sprintf("unknown devices subcommand %q", cmd)}
	}
}

// registryFlags holds the flags every devices subcommand shares. The registry
// sits in the state directory, like the device identity and checkpoints.
// Callers add their own flags, then parse.
type registryFlags struct {
	c     *commandFlagSet
	state *string
}

func newRegistryFlags(spec commandSpec) (*registryFlags, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	f := &registryFlags{c: newCommandFlags(spec)}
	f.state = f.c.fs.String("state", filepath.Join(home, ".ohya"),
		"state `directory` holding the device registry")
	return f, nil
}

// openRegistry opens the registry database in the state directory.
func (f *registryFlags) openRegistry() (*registry.DB, error) {
	return registry.Open(filepath.Join(*f.state, registry.FileName))
}

// devicesInviteSpec is the invite usage template.
var devicesInviteSpec = commandSpec{
	name:      "ohya devices invite",
	synopsis:  "issue a one-time join code",
	usageLine: "ohya devices invite [flags]",
	description: "Issues one join code and prints it on the first stdout line, so it can be piped to the joining device. " +
		"The code carries the pairing invitation plus a single-use coordinator admission key; each half is consumable exactly once within the code's lifetime. " +
		"Run it on the hub; the device joins with ohya join <code>.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya devices invite --ttl 15m"},
}

// runDevicesInvite issues one join code and prints it on the first stdout
// line, so it can be piped to the joining device. The code carries the T018
// pairing invitation plus the single-use coordinator preauth key minted with
// it — the bridge from the registry's admission to tailnet admission — so
// one code hands one device everything `ohya join` needs, and each half is
// consumable exactly once within the same lifetime.
func runDevicesInvite(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags(devicesInviteSpec)
	if err != nil {
		return err
	}
	ttl := flags.c.fs.Duration("ttl", registry.DefaultInvitationTTL,
		"how long the join code stays consumable")
	if err := flags.c.parse(args, stdout); err != nil {
		return err
	}
	if flags.c.fs.NArg() > 0 {
		return flags.c.usageErrorf("unexpected argument %q", flags.c.fs.Arg(0))
	}
	if *ttl <= 0 {
		return flags.c.usageErrorf("--ttl must be positive")
	}

	// The coordination plane first: without it there is no admission to
	// bridge the invitation to, and no half a code should be issued.
	ctl, err := control.Open(filepath.Join(*flags.state, control.DirName))
	if err != nil {
		return err
	}
	defer ctl.Close()
	st, err := ctl.State()
	if err != nil {
		return err
	}
	user, err := control.EnsureUser(st)
	if err != nil {
		return err
	}
	authKey, err := control.MintAuthKey(st, user, *ttl)
	if err != nil {
		return err
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
	fmt.Fprintln(stdout, inv.Secret+":"+authKey)
	fmt.Fprintf(stderr, "expires %s (one device may use each half once); join with: ohya join <code>\n",
		msTime(inv.ExpiresAtMS))
	return nil
}

// devicesListSpec is the list usage template.
var devicesListSpec = commandSpec{
	name:        "ohya devices list",
	synopsis:    "list devices and invitations",
	usageLine:   "ohya devices list [flags]",
	description: "Prints every paired device with its state and pairing time, then every invitation: used by which device, expired, or open until when.",
	argsTitle:   "Arguments",
	args:        "(none)",
	examples:    []string{"ohya devices list"},
}

// runDevicesList prints every paired device and every invitation.
func runDevicesList(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags(devicesListSpec)
	if err != nil {
		return err
	}
	if err := flags.c.parse(args, stdout); err != nil {
		return err
	}
	if flags.c.fs.NArg() > 0 {
		return flags.c.usageErrorf("unexpected argument %q", flags.c.fs.Arg(0))
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

// devicesRevokeSpec is the revoke usage template.
var devicesRevokeSpec = commandSpec{
	name:        "ohya devices revoke",
	synopsis:    "revoke a device, final for its identity",
	usageLine:   "ohya devices revoke <device-id>",
	description: "Revokes one device. Revocation is final for the device identity: its connections close, its sources stay bound to the revoked identity, and re-admission needs a new device identity.",
	argsTitle:   "Arguments",
	args:        "<device-id>  the device ID shown by ohya devices list",
	examples:    []string{"ohya devices revoke <device-id>"},
}

// runDevicesRevoke revokes one device. Revocation is final for the device
// identity; its connections close and re-admission needs a new identity.
func runDevicesRevoke(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags(devicesRevokeSpec)
	if err != nil {
		return err
	}
	if err := flags.c.parse(args, stdout); err != nil {
		return err
	}
	if flags.c.fs.NArg() != 1 {
		return flags.c.usageErrorf("revoke needs exactly one device ID; see ohya devices list")
	}
	deviceID := flags.c.fs.Arg(0)
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
