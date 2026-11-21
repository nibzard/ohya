package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/transport/address"
)

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

func (f *registryFlags) openRegistry() (*registry.DB, error) {
	reg, err := registry.Open(filepath.Join(*f.state, registry.FileName))
	if err != nil {
		return nil, classify(err, "registry_open_failed", "check that the --state directory exists and is writable")
	}
	return reg, nil
}

var devicesInviteSpec = commandSpec{
	name:      "ohya devices invite",
	synopsis:  "issue a one-time join code",
	usageLine: "ohya devices invite [flags]",
	description: "Issues one join code and prints it on the first stdout line, so it can be piped to the joining device. " +
		"The code is ohya1.<invitation>.<tailcat-address>: the invitation admits exactly one device within the code's lifetime, and the address holds the hub's pre-shared key, so the code is a secret. " +
		"Run it on the hub; the device joins with ohya join <code>.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya devices invite --ttl 15m"},
}

func runDevicesInvite(args []string, stdout, stderr io.Writer) error {
	flags, err := newRegistryFlags(devicesInviteSpec)
	if err != nil {
		return err
	}
	ttl := flags.c.fs.Duration("ttl", registry.DefaultInvitationTTL,
		"how long the invitation in the join code stays consumable")
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
	fmt.Fprintf(stderr, "expires %s (one device may use the invitation once; the hub address does not expire); join with: ohya join <code>\n",
		msTime(expiresAtMS))
	if *flags.json {
		return printJSON(stdout, devicesInviteSpec.name,
			inviteJSONData{Code: code, ExpiresAt: msTime(expiresAtMS)})
	}
	fmt.Fprintln(stdout, code)
	return nil
}

type inviteJSONData struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

func (f *registryFlags) issueJoinCode(ttl time.Duration) (code string, expiresAtMS int64, err error) {
	return issueJoinCode(*f.state, ttl)
}

func issueJoinCode(state string, ttl time.Duration) (code string, expiresAtMS int64, err error) {
	hubAddr, err := address.LoadHubAddr(state)
	if err != nil {
		if errors.Is(err, address.ErrNoIdentity) {
			return "", 0, classify(err, "hub_identity_absent",
				"run ohya serve once to create the hub identity, then retry")
		}
		return "", 0, err
	}
	reg, err := registry.Open(filepath.Join(state, registry.FileName))
	if err != nil {
		return "", 0, classify(err, "registry_open_failed", "check that the --state directory exists and is writable")
	}
	defer reg.Close()
	inv, err := reg.Issue(context.Background(), ttl)
	if err != nil {
		return "", 0, err
	}
	return formatJoinCode(inv.Secret, hubAddr), inv.ExpiresAtMS, nil
}

var devicesListSpec = commandSpec{
	name:        "ohya devices list",
	synopsis:    "list devices and invitations",
	usageLine:   "ohya devices list [flags]",
	description: "Prints every paired device with its state, pairing time, and the transport path and time of its last sync, then every invitation: used by which device, expired, or open until when.",
	argsTitle:   "Arguments",
	args:        "(none)",
	examples:    []string{"ohya devices list"},
}

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

	s := styleFor(stdout)
	fmt.Fprintln(stdout, s.section("Devices"))
	if len(devices) == 0 {
		fmt.Fprintf(stdout, "  %s\n", s.dim("(none paired)"))
	} else {
		tbl := cliTable{indent: "  "}
		for _, d := range devices {
			state := s.green(d.State)
			if d.State == "revoked" {
				state = s.red(d.State)
			}
			details := fmt.Sprintf("paired %s", msTime(d.PairedAtMS))
			if d.LastSyncAtMS != 0 {
				details += fmt.Sprintf(", last sync %s via %s", msTime(d.LastSyncAtMS), d.LastPath)
			} else {
				details += ", never synced"
			}
			if d.RevokedAtMS != 0 {
				details += fmt.Sprintf(", revoked %s", msTime(d.RevokedAtMS))
			}
			tbl.addRow(s.bold(d.DeviceID), state, details)
		}
		tbl.render(stdout, s)
	}

	fmt.Fprintf(stdout, "\n%s\n", s.section("Invitations"))
	if len(invitations) == 0 {
		fmt.Fprintf(stdout, "  %s\n", s.dim("(none issued)"))
	} else {
		tbl := cliTable{indent: "  "}
		for _, inv := range invitations {
			switch {
			case inv.State == "used":
				tbl.addRow(s.dim(inv.ID), s.dim("used by "+inv.UsedBy))
			case inv.Expired:
				tbl.addRow(s.dim(inv.ID), s.yellow("expired"))
			default:
				tbl.addRow(s.bold(inv.ID), s.green("open until "+msTime(inv.ExpiresAtMS)))
			}
		}
		tbl.render(stdout, s)
	}
	return nil
}

type listJSONData struct {
	Devices     []deviceJSON     `json:"devices"`
	Invitations []invitationJSON `json:"invitations"`
}

type deviceJSON struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	PairedAt   string  `json:"paired_at"`
	RevokedAt  string  `json:"revoked_at,omitempty"`
	LastPath   *string `json:"last_path"`
	LastSyncAt *string `json:"last_sync_at"`
}

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
		if d.LastSyncAtMS != 0 {
			at := msTime(d.LastSyncAtMS)
			path := d.LastPath
			j.LastSyncAt = &at
			j.LastPath = &path
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

var devicesRevokeSpec = commandSpec{
	name:        "ohya devices revoke",
	synopsis:    "revoke a device, final for its identity",
	usageLine:   "ohya devices revoke <device-id> [flags]",
	description: "Revokes one device. Revocation is final for the device identity: its connections close, its sources stay bound to the revoked identity, and re-admission needs a new device identity.",
	argsTitle:   "Arguments",
	args:        "<device-id>  the device ID shown by ohya devices list",
	examples:    []string{"ohya devices revoke <device-id>"},
}

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

type revokeJSONData struct {
	DeviceID string `json:"device_id"`
}

func msTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
