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

// registryFlags holds the flags every devices subcommand shares: the state
// directory and --json. The registry sits in the state directory, like the
// device identity and checkpoints. Callers add their own flags, then parse.
type registryFlags struct {
	c     *commandFlagSet
	state *string
	json  *bool
}

func newRegistryFlags(spec commandSpec) (*registryFlags, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	f := &registryFlags{c: newCommandFlags(spec)}
	f.state = f.c.fs.String("state", filepath.Join(home, ".ohya"),
		"state `directory` holding the device registry")
	f.json = f.c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	return f, nil
}

// openRegistry opens the registry database in the state directory.
func (f *registryFlags) openRegistry() (*registry.DB, error) {
	reg, err := registry.Open(filepath.Join(*f.state, registry.FileName))
	if err != nil {
		return nil, classify(err, "registry_open_failed", "check that the --state directory exists and is writable")
	}
	return reg, nil
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
// line, so it can be piped to the joining device — or, with --json, in one
// ADR 0005 envelope whose data carries the code, because the code is the
// output the operator asked for. The code carries the T018 pairing
// invitation plus the single-use coordinator preauth key minted with it —
// the bridge from the registry's admission to tailnet admission — so one
// code hands one device everything `ohya join` needs, and each half is
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

	code, expiresAtMS, err := flags.issueJoinCode(*ttl)
	if err != nil {
		return jsonFailurePrint(*flags.json, stdout, devicesInviteSpec.name, err)
	}
	fmt.Fprintf(stderr, "expires %s (one device may use each half once); join with: ohya join <code>\n",
		msTime(expiresAtMS))
	if *flags.json {
		return printJSON(stdout, devicesInviteSpec.name,
			inviteJSONData{Code: code, ExpiresAt: msTime(expiresAtMS)})
	}
	fmt.Fprintln(stdout, code)
	return nil
}

// inviteJSONData is the invite command's data payload. The join code lives
// here and only here: a failure envelope never carries one, because a failed
// invite mints no code (ADR 0005).
type inviteJSONData struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

// issueJoinCode runs the invite operation: coordination plane, user, preauth
// key, then registry invitation. The coordination plane first: without it
// there is no admission to bridge the invitation to, and no half a code
// should be issued.
func (f *registryFlags) issueJoinCode(ttl time.Duration) (code string, expiresAtMS int64, err error) {
	ctl, err := control.Open(filepath.Join(*f.state, control.DirName))
	if err != nil {
		return "", 0, err
	}
	defer ctl.Close()
	st, err := ctl.State()
	if err != nil {
		return "", 0, err
	}
	user, err := control.EnsureUser(st)
	if err != nil {
		return "", 0, err
	}
	authKey, err := control.MintAuthKey(st, user, ttl)
	if err != nil {
		return "", 0, err
	}
	reg, err := f.openRegistry()
	if err != nil {
		return "", 0, err
	}
	defer reg.Close()
	inv, err := reg.Issue(context.Background(), ttl)
	if err != nil {
		return "", 0, err
	}
	return inv.Secret + ":" + authKey, inv.ExpiresAtMS, nil
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

// runDevicesList prints every paired device and every invitation — as text,
// or with --json as one ADR 0005 envelope.
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
		return jsonFailurePrint(*flags.json, stdout, devicesListSpec.name, err)
	}
	defer reg.Close()
	ctx := context.Background()

	devices, err := reg.List(ctx)
	if err != nil {
		return jsonFailurePrint(*flags.json, stdout, devicesListSpec.name, err)
	}
	invitations, err := reg.ListInvitations(ctx)
	if err != nil {
		return jsonFailurePrint(*flags.json, stdout, devicesListSpec.name, err)
	}
	if *flags.json {
		return printJSON(stdout, devicesListSpec.name, listJSONData{
			Devices:     deviceJSONList(devices),
			Invitations: invitationJSONList(invitations),
		})
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

// listJSONData is the devices list command's data payload (ADR 0005). Empty
// collections are [] so a consumer never meets null.
type listJSONData struct {
	Devices     []deviceJSON     `json:"devices"`
	Invitations []invitationJSON `json:"invitations"`
}

// deviceJSON is one paired device: its ID, state, and pairing time, plus the
// revocation time on a revoked device.
type deviceJSON struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	PairedAt  string `json:"paired_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// invitationJSON is one invitation: its ID, state, and timestamps, the
// consuming device when used, and the derived expiry flag.
type invitationJSON struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	UsedBy    string `json:"used_by,omitempty"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Expired   bool   `json:"expired"`
}

func deviceJSONList(devices []registry.DeviceInfo) []deviceJSON {
	out := make([]deviceJSON, 0, len(devices))
	for _, d := range devices {
		j := deviceJSON{
			ID:       d.DeviceID,
			State:    d.State,
			PairedAt: msTime(d.PairedAtMS),
		}
		if d.RevokedAtMS != 0 {
			j.RevokedAt = msTime(d.RevokedAtMS)
		}
		out = append(out, j)
	}
	return out
}

func invitationJSONList(invitations []registry.InvitationInfo) []invitationJSON {
	out := make([]invitationJSON, 0, len(invitations))
	for _, inv := range invitations {
		out = append(out, invitationJSON{
			ID:        inv.ID,
			State:     inv.State,
			UsedBy:    inv.UsedBy,
			CreatedAt: msTime(inv.CreatedAtMS),
			ExpiresAt: msTime(inv.ExpiresAtMS),
			Expired:   inv.Expired,
		})
	}
	return out
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
		return jsonFailurePrint(*flags.json, stdout, devicesRevokeSpec.name, err)
	}
	defer reg.Close()
	if err := reg.Revoke(context.Background(), deviceID); err != nil {
		return jsonFailurePrint(*flags.json, stdout, devicesRevokeSpec.name, err)
	}
	if *flags.json {
		return printJSON(stdout, devicesRevokeSpec.name, revokeJSONData{DeviceID: deviceID})
	}
	fmt.Fprintln(stdout, deviceID, "revoked")
	return nil
}

// revokeJSONData is the revoke command's data payload (ADR 0005).
type revokeJSONData struct {
	DeviceID string `json:"device_id"`
}

// msTime renders a Unix-millisecond timestamp as UTC RFC 3339.
func msTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
