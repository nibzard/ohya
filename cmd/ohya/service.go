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

	"github.com/nibzard/ohya/internal/setup"
)

var serviceSpec = commandSpec{
	name:      "ohya service",
	synopsis:  "control the owned ohya background user service",
	usageLine: "ohya service <command> [flags]",
	description: "Inspects, starts, stops, or restarts the user service owned by ohya setup (launchd on macOS, systemd --user on Linux). " +
		"It never writes or overwrites service unit files, preserving user edits, and serializes against ohya setup through the state directory lock. " +
		"By default it targets the memory service (ingest or join); pass --hub to target the hub serving service (ohya serve).",
	argsTitle: "Commands",
	args: `status   report whether the owned service is installed, loaded, and running
start    start the owned service if it is not already running
stop     stop the owned service if it is loaded or running
restart  stop and start the owned service`,
	examples: []string{
		"ohya service status --json",
		"ohya service stop",
		"ohya service start",
		"ohya service restart --hub",
	},
}

var (
	serviceStatusSpec = commandSpec{
		name:        "ohya service status",
		synopsis:    "report the owned service state",
		usageLine:   "ohya service status [flags]",
		description: "Reads the setup ownership receipt and queries the user service manager without modifying files or starting processes.",
		argsTitle:   "Arguments",
		args:        "(none)",
		examples:    []string{"ohya service status", "ohya service status --hub --json"},
	}
	serviceStartSpec = commandSpec{
		name:        "ohya service start",
		synopsis:    "start the owned user service",
		usageLine:   "ohya service start [flags]",
		description: "Starts the service configured by ohya setup and waits until the service manager reports a running process. Already-running services are a no-op.",
		argsTitle:   "Arguments",
		args:        "(none)",
		examples:    []string{"ohya service start", "ohya service start --hub --json"},
	}
	serviceStopSpec = commandSpec{
		name:        "ohya service stop",
		synopsis:    "stop the owned user service",
		usageLine:   "ohya service stop [flags]",
		description: "Stops the service configured by ohya setup without removing its unit file, receipt, or stored memory. Already-stopped services are a no-op.",
		argsTitle:   "Arguments",
		args:        "(none)",
		examples:    []string{"ohya service stop", "ohya service stop --hub --json"},
	}
	serviceRestartSpec = commandSpec{
		name:        "ohya service restart",
		synopsis:    "restart the owned user service",
		usageLine:   "ohya service restart [flags]",
		description: "Stops and starts the service configured by ohya setup and waits until the service manager reports a running process.",
		argsTitle:   "Arguments",
		args:        "(none)",
		examples:    []string{"ohya service restart", "ohya service restart --hub --json"},
	}
)

var serviceControlHooks struct {
	loaded func(context.Context, []string) error
	run    func(context.Context, []string) error
	wait   func(context.Context, setup.Service, func(context.Context, []string) error) error
}

func runServiceCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		writeUsage(stdout, serviceSpec, nil)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "status":
		return runServiceAction(setup.ServiceActionStatus, serviceStatusSpec, rest, stdout)
	case "start":
		return runServiceAction(setup.ServiceActionStart, serviceStartSpec, rest, stdout)
	case "stop":
		return runServiceAction(setup.ServiceActionStop, serviceStopSpec, rest, stdout)
	case "restart":
		return runServiceAction(setup.ServiceActionRestart, serviceRestartSpec, rest, stdout)
	default:
		return usageError{cmd: "ohya service", msg: fmt.Sprintf("unknown service subcommand %q", cmd)}
	}
}

func runServiceAction(action setup.ServiceAction, spec commandSpec, args []string, stdout io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(spec)
	state := c.fs.String("state", filepath.Join(home, ".ohya"),
		"setup state `directory` holding the service ownership receipt")
	hub := c.fs.Bool("hub", false,
		"target the hub serving service (ohya serve) instead of the memory service")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := setup.ControlService(ctx, action, setup.ServiceControlOptions{
		State:         *state,
		Home:          home,
		Hub:           *hub,
		LoadedService: serviceControlHooks.loaded,
		RunService:    serviceControlHooks.run,
		Wait:          serviceControlHooks.wait,
	})
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, spec.name, classifyServiceError(err))
	}
	if *asJSON {
		return printJSON(stdout, spec.name, res)
	}
	writeServiceStatusText(stdout, res)
	return nil
}

func classifyServiceError(err error) error {
	switch {
	case errors.Is(err, setup.ErrServiceNotConfigured):
		return classify(err, "service_not_configured", "run ohya setup (or ohya setup --hub for the hub service) first")
	case errors.Is(err, setup.ErrServiceUnitMissing):
		return classify(err, "service_unit_missing", "run ohya setup to recreate the service unit")
	default:
		return classify(err, "service_control_failed", "check your user service manager and run ohya doctor")
	}
}

func writeServiceStatusText(w io.Writer, res setup.ServiceStatus) {
	line := fmt.Sprintf("%s %s: installed=%t loaded=%t running=%t", res.Service, res.Action, res.Installed, res.Loaded, res.Running)
	if res.Action != string(setup.ServiceActionStatus) {
		line += fmt.Sprintf(" changed=%t", res.Changed)
	}
	if res.Edited {
		line += " edited=true"
	}
	if res.Detail != "" {
		line += " (" + res.Detail + ")"
	}
	fmt.Fprintln(w, line)
}
