package address

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/derp/derpserver"
	"tailscale.com/envknob"
	"tailscale.com/net/netcheck"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func TestMain(m *testing.M) {
	envknob.Setenv("IN_TS_TEST", "true")
	netcheck.HookStartCaptivePortalDetection.SetForTest(func(ctx context.Context, c *netcheck.Client, dm *tailcfg.DERPMap, preferredDERP tailcfg.DERPRegionID, setCaptivePortal func(bool)) (done <-chan struct{}, stop func()) {
		return syncs.ClosedChan(), func() {}
	})
	os.Exit(m.Run())
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func startLocalDERP(t *testing.T) *tailcfg.DERPRegion {
	t.Helper()
	srv := derpserver.New(key.NewNode(), logger.Discard)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the DERP relay: %v", err)
	}
	httpsrv := httptest.NewUnstartedServer(derpserver.Handler(srv))
	httpsrv.Listener.Close()
	httpsrv.Listener = ln
	httpsrv.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	httpsrv.StartTLS()
	t.Cleanup(func() {
		httpsrv.CloseClientConnections()
		httpsrv.Close()
		srv.Close()
	})
	port := httpsrv.Listener.Addr().(*net.TCPAddr).Port
	return &tailcfg.DERPRegion{
		RegionID:   1,
		RegionCode: "ohyatest",
		RegionName: "ohya local test relay",
		Nodes: []*tailcfg.DERPNode{{
			Name:             "ohyatest1",
			RegionID:         1,
			HostName:         "127.0.0.1",
			IPv4:             "127.0.0.1",
			IPv6:             "none",
			DERPPort:         port,
			InsecureForTests: true,
		}},
	}
}

func serveEcho(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				io.Copy(conn, conn)
			}(conn)
		}
	}()
}

func exchange(t *testing.T, conn net.Conn, message string) {
	t.Helper()
	if _, err := conn.Write([]byte(message + "\n")); err != nil {
		t.Fatalf("write over the tunnel: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read over the tunnel: %v", err)
	}
	if line != message+"\n" {
		t.Fatalf("echo = %q, want %q", line, message+"\n")
	}
}

func TestHubKeepsAddressAcrossRestart(t *testing.T) {
	region := startLocalDERP(t)
	state := t.TempDir()

	hubLog := &logSink{}
	hub, err := NewHub(HubOptions{StateDir: state, Region: region, Logf: hubLog.Logf})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	hostPort9027, err := net.Listen("tcp", "127.0.0.1:9027")
	if err != nil {
		t.Fatalf("preflight bind of host port 9027: %v", err)
	}
	hostPort9027.Close()
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if port := ln.Addr().(*net.TCPAddr).Port; port != HubPort {
		t.Fatalf("listener port = %d, want %d", port, HubPort)
	}
	stillFree, err := net.Listen("tcp", "127.0.0.1:9027")
	if err != nil {
		t.Fatalf("hub Listen must not bind host port 9027: %v", err)
	}
	stillFree.Close()
	addr := hub.Addr()
	if !strings.HasPrefix(addr, "tc") {
		t.Fatalf("address %q must be a tailcat address", addr)
	}
	public := hub.PublicKey()
	if !strings.HasPrefix(public, "nodekey:") {
		t.Fatalf("PublicKey = %q, want a nodekey: prefix", public)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	identityPath := filepath.Join(state, hubStateDir, identityFileName)
	data, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	info, err := os.Stat(identityPath)
	if err != nil {
		t.Fatalf("stat identity: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("identity permissions = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Join(state, hubStateDir))
	if err != nil {
		t.Fatalf("stat identity directory: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("identity directory permissions = %o, want 700", perm)
	}

	hub2, err := NewHub(HubOptions{StateDir: state, Region: region, Logf: hubLog.Logf})
	if err != nil {
		t.Fatalf("NewHub after restart: %v", err)
	}
	ln2, err := hub2.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen after restart: %v", err)
	}
	defer hub2.Close()
	if hub2.Addr() != addr {
		t.Fatalf("address after restart = %q, want the saved %q", hub2.Addr(), addr)
	}
	if hub2.PublicKey() != public {
		t.Fatalf("PublicKey after restart = %q, want %q", hub2.PublicKey(), public)
	}
	data2, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity after restart: %v", err)
	}
	if string(data2) != string(data) {
		t.Fatalf("identity changed across a restart that loaded it")
	}
	ln2.Close()

	logs := hubLog.all()
	if strings.Contains(logs, addr) {
		t.Fatalf("the hub address appears in hub logs")
	}
	if strings.Contains(logs, "privkey:") || strings.Contains(logs, "psk:") {
		t.Fatalf("key material appears in hub logs")
	}
}

func TestHubDeviceExchange(t *testing.T) {
	t.Parallel()
	region := startLocalDERP(t)
	state := t.TempDir()

	hubLog := &logSink{}
	hub, err := NewHub(HubOptions{StateDir: state, Region: region, Logf: hubLog.Logf})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveEcho(t, ln)
	addr := hub.Addr()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deviceLog := &logSink{}
	device := NewDevice(DeviceOptions{Logf: deviceLog.Logf})
	conn, err := device.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	exchange(t, conn, "first device says hello")
	if err := conn.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}

	secondLog := &logSink{}
	second := NewDevice(DeviceOptions{Logf: secondLog.Logf})
	conn2, err := second.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial with a second device key: %v", err)
	}
	exchange(t, conn2, "second device says hello")
	conn2.Close()

	badLog := &logSink{}
	bad := NewDevice(DeviceOptions{Logf: badLog.Logf})
	_, err = bad.Dial(ctx, "tcnot-a-tailcat-address")
	if err == nil {
		t.Fatalf("dial with an invalid address must fail")
	}
	if strings.Contains(err.Error(), "tcnot-a-tailcat-address") {
		t.Fatalf("the dial error echoes the address text: %v", err)
	}
	if err := bad.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	device.Close()
	second.Close()
	if err := hub.Close(); err != nil {
		t.Fatalf("Close hub: %v", err)
	}

	hub2, err := NewHub(HubOptions{StateDir: state, Region: region, Logf: hubLog.Logf})
	if err != nil {
		t.Fatalf("NewHub after restart: %v", err)
	}
	ln2, err := hub2.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen after restart: %v", err)
	}
	serveEcho(t, ln2)
	if hub2.Addr() != addr {
		t.Fatalf("address changed across restart: %q then %q", addr, hub2.Addr())
	}
	thirdLog := &logSink{}
	third := NewDevice(DeviceOptions{Logf: thirdLog.Logf})
	conn3, err := third.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial after hub restart: %v", err)
	}
	exchange(t, conn3, "third device after restart")
	conn3.Close()
	third.Close()
	hub2.Close()

	for name, logs := range map[string]string{
		"hub":     hubLog.all(),
		"device":  deviceLog.all(),
		"second":  secondLog.all(),
		"third":   thirdLog.all(),
		"badDial": badLog.all(),
	} {
		if strings.Contains(logs, addr) {
			t.Fatalf("the hub address appears in %s logs", name)
		}
		if strings.Contains(logs, "privkey:") || strings.Contains(logs, "psk:") {
			t.Fatalf("key material appears in %s logs", name)
		}
	}
}

func TestHubRejectsRegionChangeAfterIdentity(t *testing.T) {
	region := startLocalDERP(t)
	state := t.TempDir()

	hub, err := NewHub(HubOptions{StateDir: state, Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.Close()
	hub.Close()

	other := &tailcfg.DERPRegion{RegionID: 2, RegionCode: "other", Nodes: region.Nodes}
	_, err = NewHub(HubOptions{StateDir: state, Region: other})
	if err == nil {
		t.Fatalf("NewHub must refuse a different region once the identity exists")
	}
	if !strings.Contains(err.Error(), filepath.Join(state, hubStateDir, identityFileName)) {
		t.Fatalf("error must name the identity file: %v", err)
	}
}

func TestHubCorruptIdentity(t *testing.T) {
	region := startLocalDERP(t)
	valid, err := json.Marshal(identity{
		Version: identityVersion,
		Node:    key.NewNode(),
		PSK:     tailcat.NewPresharedKey(),
		Region:  region,
	})
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	nodeText, err := key.NewNode().MarshalText()
	if err != nil {
		t.Fatalf("marshal node key: %v", err)
	}
	pskText, err := tailcat.NewPresharedKey().MarshalText()
	if err != nil {
		t.Fatalf("marshal pre-shared key: %v", err)
	}
	missingRegion := fmt.Sprintf(`{"version":1,"node":%q,"preshared_key":%q}`, string(nodeText), string(pskText))

	cases := []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		{"garbage", []byte("\x00\x01 not json at all"), 0o600},
		{"future version", []byte(`{"version":99}`), 0o600},
		{"missing region", []byte(missingRegion), 0o600},
		{"marked psk", []byte(strings.Replace(string(valid), `"preshared_key":`, `"preshared_key":"psk:OHYA-SECRET-MARKER-DO-NOT-ECHO","old_preshared_key":`, 1)), 0o600},
		{"group readable", valid, 0o640},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			path := filepath.Join(state, hubStateDir, identityFileName)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(path, tc.content, tc.mode); err != nil {
				t.Fatalf("write identity: %v", err)
			}

			hub, err := NewHub(HubOptions{StateDir: state, Region: region})
			if err == nil {
				hub.Close()
				t.Fatalf("NewHub must fail on a corrupt identity file")
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error must name the identity file %s: %v", path, err)
			}
			if strings.Contains(err.Error(), "OHYA-SECRET-MARKER-DO-NOT-ECHO") {
				t.Fatalf("error echoes file content: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read identity after the failed start: %v", err)
			}
			if string(got) != string(tc.content) {
				t.Fatalf("NewHub overwrote the identity file on failure")
			}
		})
	}
}

func TestHubRequiresStateDir(t *testing.T) {
	if _, err := NewHub(HubOptions{}); err == nil {
		t.Fatalf("NewHub must require a state directory")
	}
	if _, err := NewHub(HubOptions{StateDir: "  "}); err == nil {
		t.Fatalf("NewHub must reject a blank state directory")
	}
}

func TestDeviceRejectsSecondHub(t *testing.T) {
	t.Parallel()
	region := startLocalDERP(t)
	hub, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveEcho(t, ln)
	defer hub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	device := NewDevice(DeviceOptions{})
	defer device.Close()
	conn, err := device.Dial(ctx, hub.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	exchange(t, conn, "first hub")
	conn.Close()

	other, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln2, err := other.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln2.Close()
	other.Close()

	if _, err := device.Dial(ctx, other.Addr()); err == nil {
		t.Fatalf("Dial must refuse a second hub address on one device")
	}
}

func TestLoadHubAddrMatchesStartedHub(t *testing.T) {
	region := startLocalDERP(t)
	state := t.TempDir()

	if _, err := LoadHubAddr(state); err == nil {
		t.Fatalf("LoadHubAddr without an identity must fail")
	}

	hub, err := NewHub(HubOptions{StateDir: state, Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer hub.Close()
	ln.Close()

	loaded, err := LoadHubAddr(state)
	if err != nil {
		t.Fatalf("LoadHubAddr: %v", err)
	}
	if loaded != hub.Addr() {
		t.Fatalf("LoadHubAddr = %q, want the started hub address %q", loaded, hub.Addr())
	}
	if r := hub.Region(); r == nil || r.RegionID != region.RegionID {
		t.Fatalf("hub Region = %+v, want the saved region", r)
	}
}

func TestValidateRegion(t *testing.T) {
	node := func(mut func(*tailcfg.DERPNode)) *tailcfg.DERPNode {
		n := &tailcfg.DERPNode{HostName: "derp.example", DERPPort: 443}
		if mut != nil {
			mut(n)
		}
		return n
	}
	for _, tc := range []struct {
		name   string
		region *tailcfg.DERPRegion
		ok     bool
	}{
		{"nil", nil, false},
		{"zero region id", &tailcfg.DERPRegion{RegionID: 0, Nodes: []*tailcfg.DERPNode{node(nil)}}, false},
		{"no nodes", &tailcfg.DERPRegion{RegionID: 1}, false},
		{"stun only", &tailcfg.DERPRegion{RegionID: 1, Nodes: []*tailcfg.DERPNode{node(func(n *tailcfg.DERPNode) { n.STUNOnly = true })}}, false},
		{"null node", &tailcfg.DERPRegion{RegionID: 1, Nodes: []*tailcfg.DERPNode{nil}}, false},
		{"no host name", &tailcfg.DERPRegion{RegionID: 1, Nodes: []*tailcfg.DERPNode{node(func(n *tailcfg.DERPNode) { n.HostName = "" })}}, false},
		{"valid", &tailcfg.DERPRegion{RegionID: 1, Nodes: []*tailcfg.DERPNode{node(nil)}}, true},
	} {
		err := validateRegion(tc.region)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestDeviceFailedDialDoesNotKillSiblingConnection(t *testing.T) {
	t.Parallel()
	region := startLocalDERP(t)
	state := t.TempDir()

	hub, err := NewHub(HubOptions{StateDir: state, Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer hub.Close()
	serveEcho(t, ln)
	addr := hub.Addr()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	device := NewDevice(DeviceOptions{})
	defer device.Close()

	conn, err := device.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	exchange(t, conn, "before sibling failure")

	canceled, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, err := device.Dial(canceled, addr); err == nil {
		t.Fatal("canceled dial succeeded")
	}

	expired, ecancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer ecancel()
	if _, err := device.Dial(expired, addr); err == nil {
		t.Fatal("expired dial succeeded")
	}

	exchange(t, conn, "after sibling failure")
}

func TestHubAndDeviceLifecycleGuards(t *testing.T) {
	region := startLocalDERP(t)
	hub, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
	if err != nil {
		t.Fatal(err)
	}
	if report := hub.PeerPath(nil); report.Path != PathUnknown {
		t.Fatalf("PeerPath(nil) = %+v", report)
	}
	if report := hub.PeerPath(&net.TCPAddr{IP: net.ParseIP("fd7a:115c:a1e0::1"), Port: 1234}); report.Path != PathUnknown {
		t.Fatalf("PeerPath before Listen = %+v", report)
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hub.Drain(context.Background()); err != nil {
		t.Fatalf("Hub.Drain after Close: %v", err)
	}
	dev := NewDevice(DeviceOptions{})
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dev.Drain(context.Background()); err != nil {
		t.Fatalf("Device.Drain after Close: %v", err)
	}
}

func TestProbeRelayEndpointsFallsBackPastDownNode(t *testing.T) {
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	closedLn.Close()

	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer liveLn.Close()
	go func() {
		for {
			c, err := liveLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	livePort := liveLn.Addr().(*net.TCPAddr).Port

	relays, err := relayEndpoints(&tailcfg.DERPRegion{
		Nodes: []*tailcfg.DERPNode{
			{HostName: "127.0.0.1", DERPPort: closedPort},
			{HostName: "127.0.0.1", DERPPort: livePort},
		},
	})
	if err != nil {
		t.Fatalf("relayEndpoints: %v", err)
	}
	if err := dialAnyRelay(context.Background(), relays); err != nil {
		t.Fatalf("dialAnyRelay failed when second node was reachable: %v", err)
	}
}

func TestDeviceReleaseSlotLockedSingleCloseWithInFlightDial(t *testing.T) {
	t.Run("unit", func(t *testing.T) {
		dev := NewDevice(DeviceOptions{})
		active := dev.slot
		active.dialing = 2
		retired := &clientSlot{
			client:  &tailcat.Client{Key: key.NewNode()},
			open:    1,
			dialing: 1,
			retired: true,
		}
		dev.retired[retired] = struct{}{}

		if err := dev.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		dev.mu.Lock()
		defer dev.mu.Unlock()
		for _, slot := range []*clientSlot{active, retired} {
			if slot.closed || dev.releaseSlotLocked(slot) != nil {
				t.Fatal("client released while its dials were still in flight")
			}
			slot.dialing = 0
			if dev.releaseSlotLocked(slot) != slot.client {
				t.Fatal("last dial did not claim the client for close")
			}
			if !slot.closed || dev.releaseSlotLocked(slot) != nil {
				t.Fatal("client was released more than once")
			}
		}
		if len(dev.retired) != 0 {
			t.Fatalf("len(dev.retired) = %d; want 0", len(dev.retired))
		}
	})

	t.Run("concurrent dial and close", func(t *testing.T) {
		t.Parallel()
		region := startLocalDERP(t)
		hub, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
		if err != nil {
			t.Fatalf("NewHub: %v", err)
		}
		ln, err := hub.Listen(context.Background())
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		addr := hub.Addr()
		ln.Close()
		hub.Close()

		dev := NewDevice(DeviceOptions{})
		dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := dev.Dial(dialCtx, addr)
				if conn != nil {
					conn.Close()
				}
				errs <- err
			}()
		}

		var slot *clientSlot
		deadline := time.Now().Add(5 * time.Second)
		for {
			dev.mu.Lock()
			dialing := dev.slot.dialing
			slot = dev.slot
			dev.mu.Unlock()
			if dialing == 2 {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				wg.Wait()
				t.Fatalf("timed out waiting for two in-flight dials, got %d", dialing)
			}
			time.Sleep(5 * time.Millisecond)
		}

		if err := dev.Close(); err != nil {
			t.Fatalf("Device.Close: %v", err)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err == nil {
				t.Fatal("in-flight dial unexpectedly succeeded after hub and device close")
			}
		}

		dev.mu.Lock()
		remainingDialing := slot.dialing
		closed := slot.closed
		released := dev.releaseSlotLocked(slot)
		dev.mu.Unlock()
		if remainingDialing != 0 {
			t.Fatalf("slot.dialing = %d after dials exited; want 0", remainingDialing)
		}
		if !closed || released != nil {
			t.Fatalf("after final dial: closed = %v, released = %v; want true, nil", closed, released)
		}
	})
}

func TestDeviceDrainIncludesRetiredOpenConnections(t *testing.T) {
	t.Parallel()
	region := startLocalDERP(t)
	hub, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer hub.Close()
	serveEcho(t, ln)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dev := NewDevice(DeviceOptions{})
	defer dev.Close()

	conn, err := dev.Dial(ctx, hub.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	exchange(t, conn, "active before retire")

	dev.mu.Lock()
	slot1 := dev.slot
	slot1.dialing++
	dev.mu.Unlock()
	dev.finishDialFailure(slot1, true)

	dev.mu.Lock()
	started := dev.started
	retiredOpen := slot1.open
	dev.mu.Unlock()
	if started {
		t.Fatal("dev.started = true after finishDialFailure; want false")
	}
	if retiredOpen != 1 {
		t.Fatalf("slot1.open = %d; want 1", retiredOpen)
	}

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	if err := dev.Drain(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain with open retired slot err = %v; want context.DeadlineExceeded", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close: %v", err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := dev.Drain(drainCtx); err != nil {
		t.Fatalf("Drain after retired connection closed = %v; want nil", err)
	}
}
