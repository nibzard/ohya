package control

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/transport/tailcat"
)

// freePort reserves one free loopback TCP port and releases it, for binding
// the control listener in tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// freeUDPPort reserves one free loopback UDP port and releases it, for the
// embedded relay's STUN listener. A port free on TCP is not necessarily free
// on UDP, and a failed STUN bind is fatal inside the pinned library (it
// log.Fatal-exits the process), so the reservation must use UDP.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// shortTempDir returns a fresh temp directory whose path stays short enough
// for the plane's gRPC unix socket (<dir>/control/headscale.sock). t.TempDir
// appends the test name and a counter under the OS temp root; on macOS that
// root is /var/folders/<xx>/<per-user id>/T/, long enough to push the socket
// past darwin's 104-byte sun_path limit, so the embedded server dies at its
// gRPC listener ("bind: invalid argument") — the macOS check-job failure of
// TestControlStartServeAndMint. A flat os.MkdirTemp keeps every supported
// platform (darwin's 104 is the smallest) comfortably under the limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ohya-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestProvisionGeneratesDeterministicConfig(t *testing.T) {
	opts := Options{
		Dir:        filepath.Join(t.TempDir(), DirName),
		ServerURL:  "http://127.0.0.1:19417",
		ListenAddr: "127.0.0.1:19417",
		STUNAddr:   "127.0.0.1:13478",
		BaseDomain: "tail.ohya",
	}
	ctl, err := Provision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.URL() != opts.ServerURL {
		t.Fatalf("URL = %q, want %q", ctl.URL(), opts.ServerURL)
	}
	path := filepath.Join(opts.Dir, configFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	for _, want := range []string{
		"server_url: http://127.0.0.1:19417",
		"listen_addr: 127.0.0.1:19417",
		"enabled: true", // embedded DERP
		"stun_listen_addr: 127.0.0.1:13478",
		"base_domain: tail.ohya",
		"type: sqlite",
		"urls: []", // no external DERP maps: local-first control plane
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("generated config misses %q:\n%s", want, cfg)
		}
	}
	// The state paths must be absolute: headscale resolves relative paths
	// against the config directory, which would nest the state directory.
	if !filepath.IsAbs(opts.Dir) {
		t.Fatalf("options dir %q must resolve absolute", opts.Dir)
	}

	// Re-provisioning is deterministic: equal options, equal bytes.
	again, err := Provision(opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = again
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != cfg {
		t.Fatal("re-provisioning with equal options changed the generated config")
	}

	// Open loads the existing plane without rewriting it.
	opened, err := Open(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if opened.URL() != opts.ServerURL {
		t.Fatalf("opened URL = %q", opened.URL())
	}

	// A missing plane reports the serve remedy.
	if _, err := Open(t.TempDir()); err == nil || !strings.Contains(err.Error(), "run ohya serve once") {
		t.Fatalf("open without a plane err = %v; want the serve remedy", err)
	}

	// Invalid options are refused before any file is written.
	bad := Options{Dir: filepath.Join(t.TempDir(), DirName), ListenAddr: "127.0.0.1:1", STUNAddr: "127.0.0.1:1", BaseDomain: "x"}
	if _, err := Provision(bad); err == nil {
		t.Fatal("provision without a ServerURL was accepted")
	}
}

func TestControlStartServeAndMint(t *testing.T) {
	port := freePort(t)
	opts := Options{
		Dir:        filepath.Join(shortTempDir(t), DirName),
		ServerURL:  "http://127.0.0.1:" + strconv.Itoa(port),
		ListenAddr: "127.0.0.1:" + strconv.Itoa(port),
		STUNAddr:   "127.0.0.1:" + strconv.Itoa(freeUDPPort(t)),
		BaseDomain: tailcat.DefaultBaseDomain,
	}
	ctl, err := Provision(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := ctl.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	st, err := ctl.State()
	if err != nil {
		t.Fatal(err)
	}
	if HubEnrolled(st) {
		t.Fatal("a fresh plane reports the hub node as enrolled")
	}
	user, err := EnsureUser(st)
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != UserName {
		t.Fatalf("user = %q, want %q", user.Name, UserName)
	}
	key, err := MintAuthKey(st, user, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) < 16 || strings.ContainsAny(key, " \t\n") {
		t.Fatalf("preauth key = %q; want an opaque single-line secret", key)
	}
	// The key exists in the coordination database as single-use.
	got, err := st.GetPreAuthKey(key)
	if err != nil {
		t.Fatalf("minted key not stored: %v", err)
	}
	if got.Reusable || got.Used {
		t.Fatalf("minted key reusable=%v used=%v; want single-use and unused", got.Reusable, got.Used)
	}
}

func TestSplitJoinCode(t *testing.T) {
	inv, key, err := SplitJoinCode("abc123:def456")
	if err != nil || inv != "abc123" || key != "def456" {
		t.Fatalf("split = %q, %q, %v", inv, key, err)
	}
	for _, bad := range []string{"", "only-invitation", ":key", "inv:", "a:b:c"} {
		if _, _, err := SplitJoinCode(bad); err == nil {
			t.Fatalf("split %q accepted", bad)
		}
	}
}
