package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultListenPort = 9027

func main() {
	log.SetFlags(log.Lmicroseconds | log.LUTC)
	if len(os.Args) < 2 {
		log.Fatal("usage: tailcat-spike hub|device|pairtest [flags]")
	}

	var (
		common    flags
		hubFlags  = flag.NewFlagSet("hub", flag.ExitOnError)
		devFlags  = flag.NewFlagSet("device", flag.ExitOnError)
		pairFlags = flag.NewFlagSet("pairtest", flag.ExitOnError)
	)
	common.bind(hubFlags)
	common.bind(devFlags)
	common.bind(pairFlags)
	devFlags.StringVar(&common.Hub, "hub", "", "hub address to dial (host:port)")
	pairFlags.StringVar(&common.Hub, "hub", "", "hub address to dial (host:port)")

	switch os.Args[1] {
	case "hub":
		must(hubFlags.Parse(os.Args[2:]))
		runHub(common)
	case "device":
		must(devFlags.Parse(os.Args[2:]))
		if common.Hub == "" {
			log.Fatal("--hub is required for device")
		}
		runDevice(common)
	case "pairtest":
		must(pairFlags.Parse(os.Args[2:]))
		if common.Hub == "" {
			log.Fatal("--hub is required for pairtest")
		}
		if !runPairTest(common) {
			os.Exit(1)
		}
	default:
		log.Fatalf("unknown mode %q", os.Args[1])
	}
}

type flags struct {
	Transport string
	StateDir  string
	Control   string
	AuthKey   string
	Hostname  string
	Port      int
	Out       string
	Hub       string
	Payloads  string
	Count     int
	Timeout   time.Duration
	Ephemeral bool
}

func (f *flags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.Transport, "transport", "tailcat", "tailcat or tcp")
	fs.StringVar(&f.StateDir, "state", "", "node state directory (tailcat only)")
	fs.StringVar(&f.Control, "control", "", "coordination server URL (tailcat only)")
	fs.StringVar(&f.AuthKey, "authkey", "", "pairing key (tailcat only)")
	fs.StringVar(&f.Hostname, "hostname", "", "node hostname in the tailnet (tailcat only)")
	fs.BoolVar(&f.Ephemeral, "ephemeral", false, "register as an ephemeral node (expired at disconnect)")
	fs.IntVar(&f.Port, "port", defaultListenPort, "tailnet port the hub listens on")
	fs.StringVar(&f.Out, "out", "", "write results as JSON to this file")
	fs.StringVar(&f.Payloads, "payloads", "1024,65536,1048576", "comma-separated payload sizes in bytes")
	fs.IntVar(&f.Count, "count", 100, "exchanges per payload size")
	fs.DurationVar(&f.Timeout, "timeout", 60*time.Second, "overall timeout")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func writeResults(path string, v any) {
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("encode results: %v", err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o666); err != nil {
		log.Printf("write results: %v", err)
	}
}

func stopOnSignal() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
