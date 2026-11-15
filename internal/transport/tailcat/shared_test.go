package tailcat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"tailscale.com/net/socks5"
)

type sharedTestNode struct {
	listener net.Listener
	running  *atomic.Int32
}

func (n *sharedTestNode) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func (n *sharedTestNode) proxy() (string, string, error) {
	return n.listener.Addr().String(), strings.Repeat("a", 32), nil
}

func (n *sharedTestNode) Close() error {
	n.running.Add(-1)
	return n.listener.Close()
}

func sharedTestOptions(t *testing.T) Options {
	t.Helper()
	return Options{Hostname: "device", ControlURL: "http://127.0.0.1:9417", StateDir: t.TempDir()}
}

func sharedTestDevice(t *testing.T, opts Options, running, started *atomic.Int32) *SharedDevice {
	t.Helper()
	d, err := NewSharedDevice(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.newNode = func(Options) (sharedNode, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		if n := running.Add(1); n != 1 {
			t.Errorf("%d network stacks run for one device", n)
		}
		started.Add(1)
		server := &socks5.Server{
			Username: "tsnet", Password: strings.Repeat("a", 32),
			Logf: func(string, ...any) {},
		}
		go server.Serve(listener)
		return &sharedTestNode{listener: listener, running: running}, nil
	}
	return d
}

func sharedEchoTarget(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func sharedEcho(d *SharedDevice, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.Dial(ctx, target)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("echo")); err != nil {
		return err
	}
	var reply [4]byte
	_, err = io.ReadFull(conn, reply[:])
	if err == nil && string(reply[:]) != "echo" {
		return errors.New("unexpected echo response")
	}
	return err
}

func TestSharedDeviceOwnershipTransfer(t *testing.T) {
	opts := sharedTestOptions(t)
	var running, started atomic.Int32
	owner := sharedTestDevice(t, opts, &running, &started)
	borrower := sharedTestDevice(t, opts, &running, &started)
	other := sharedTestDevice(t, opts, &running, &started)
	target := sharedEchoTarget(t)
	for _, d := range []*SharedDevice{owner, borrower, other} {
		if err := sharedEcho(d, target); err != nil {
			t.Fatal(err)
		}
	}
	if started.Load() != 1 {
		t.Fatalf("started %d stacks, want 1", started.Load())
	}
	info, err := os.Stat(owner.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy endpoint permissions: %v, %v", info, err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sharedEcho(owner, target); err != nil {
		t.Fatalf("borrower close stopped the owner: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owner.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint after owner closes: %v", err)
	}
	if err := sharedEcho(borrower, target); err != nil {
		t.Fatalf("ownership transfer: %v", err)
	}
	if started.Load() != 2 || running.Load() != 1 {
		t.Fatalf("started=%d running=%d; want 2, 1", started.Load(), running.Load())
	}
	if _, err := owner.Dial(context.Background(), target); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed device dial: %v", err)
	}
}

func TestSharedDeviceConcurrentStartup(t *testing.T) {
	opts := sharedTestOptions(t)
	var running, started atomic.Int32
	target := sharedEchoTarget(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 5 {
		d := sharedTestDevice(t, opts, &running, &started)
		wg.Go(func() {
			<-start
			if err := sharedEcho(d, target); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if started.Load() != 1 {
		t.Fatalf("started %d stacks, want 1", started.Load())
	}
}

func TestSharedDeviceWaitForOwnerRespectsContext(t *testing.T) {
	opts := sharedTestOptions(t)
	var running, started atomic.Int32
	owner := sharedTestDevice(t, opts, &running, &started)
	if err := syscall.Flock(int(owner.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	borrower := sharedTestDevice(t, opts, &running, &started)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := borrower.Dial(ctx, "hub:9027"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for owner: %v", err)
	}
	if started.Load() != 0 {
		t.Fatal("a borrower started a network stack")
	}
}

func TestSharedDeviceRejectsMismatchedSettings(t *testing.T) {
	opts := sharedTestOptions(t)
	var running, started atomic.Int32
	owner := sharedTestDevice(t, opts, &running, &started)
	if err := sharedEcho(owner, sharedEchoTarget(t)); err != nil {
		t.Fatal(err)
	}
	opts.ControlURL = "https://other.example"
	borrower := sharedTestDevice(t, opts, &running, &started)
	if _, err := borrower.Dial(context.Background(), "hub:9027"); err == nil || !strings.Contains(err.Error(), "another coordination URL") {
		t.Fatalf("mismatched settings: %v", err)
	}
}

func TestSharedDeviceRejectsUnsafeEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		mode    os.FileMode
	}{
		{"non-loopback", "192.0.2.1:9027", 0o600},
		{"readable-by-others", "127.0.0.1:9027", 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := sharedTestOptions(t)
			var running, started atomic.Int32
			owner := sharedTestDevice(t, opts, &running, &started)
			if err := syscall.Flock(int(owner.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(proxyEndpoint{Version: 1, Address: tc.address, Password: strings.Repeat("a", 32), Hostname: opts.Hostname, ControlURL: opts.ControlURL})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(owner.path, data, tc.mode); err != nil {
				t.Fatal(err)
			}
			borrower := sharedTestDevice(t, opts, &running, &started)
			if _, err := borrower.Dial(context.Background(), "hub:9027"); err == nil {
				t.Fatal("unsafe proxy endpoint was accepted")
			}
			if started.Load() != 0 {
				t.Fatal("a borrower started a network stack")
			}
		})
	}
}

func TestSharedDeviceOwnerCrash(t *testing.T) {
	if dir := os.Getenv("OHYA_SHARED_LOCK_CHILD"); dir != "" {
		lock, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "proxy.json"), []byte("stale endpoint"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Stdout.WriteString("ready\n")
		for {
			time.Sleep(time.Hour)
		}
	}
	opts := sharedTestOptions(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedDeviceOwnerCrash$")
	cmd.Env = append(os.Environ(), "OHYA_SHARED_LOCK_CHILD="+opts.StateDir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("lock child: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock child did not become ready")
	}
	var running, started atomic.Int32
	d := sharedTestDevice(t, opts, &running, &started)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := d.Dial(ctx, "hub:9027"); err == nil {
		t.Fatal("borrower accepted a stale endpoint while the owner held the lock")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if err := sharedEcho(d, sharedEchoTarget(t)); err != nil {
		t.Fatalf("recover after owner crash: %v", err)
	}
	if started.Load() != 1 {
		t.Fatalf("started %d stacks after crash, want 1", started.Load())
	}
}

func TestSharedDeviceProxyRequiresAuthentication(t *testing.T) {
	opts := sharedTestOptions(t)
	var running, started atomic.Int32
	owner := sharedTestDevice(t, opts, &running, &started)
	target := sharedEchoTarget(t)
	if err := sharedEcho(owner, target); err != nil {
		t.Fatal(err)
	}
	endpoint, err := owner.readEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Password = strings.Repeat("b", 32)
	data, err := json.Marshal(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owner.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	borrower := sharedTestDevice(t, opts, &running, &started)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = borrower.Dial(ctx, target)
	if err == nil {
		t.Fatal("proxy accepted an incorrect password")
	}
	if strings.Contains(err.Error(), endpoint.Password) {
		t.Fatal("proxy error exposed its password")
	}
	if started.Load() != 1 {
		t.Fatal("a failed proxy authentication started another network stack")
	}
}

func TestSharedDeviceStartupFailure(t *testing.T) {
	opts := sharedTestOptions(t)
	var running, started atomic.Int32
	owner := sharedTestDevice(t, opts, &running, &started)
	factory := owner.newNode
	owner.newNode = func(Options) (sharedNode, error) { return nil, errors.New("startup failure") }
	if _, err := owner.Dial(context.Background(), "hub:9027"); err == nil {
		t.Fatal("startup succeeded")
	}
	borrower := sharedTestDevice(t, opts, &running, &started)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := borrower.Dial(ctx, "hub:9027"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("borrower waiting for failed startup: %v", err)
	}
	owner.newNode = factory
	if err := sharedEcho(owner, sharedEchoTarget(t)); err != nil {
		t.Fatalf("retry startup: %v", err)
	}
	if started.Load() != 1 {
		t.Fatalf("started %d nodes after retry; want 1", started.Load())
	}
}

type staleDialNode struct {
	listener net.Listener
	fail     bool
	closed   chan struct{}
	running  *atomic.Int32
}

func (n *staleDialNode) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if n.fail {
		return nil, errors.New("stale peer state")
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func (n *staleDialNode) proxy() (string, string, error) {
	return n.listener.Addr().String(), strings.Repeat("a", 32), nil
}

func (n *staleDialNode) Close() error {
	select {
	case <-n.closed:
	default:
		close(n.closed)
	}
	if n.running != nil {
		n.running.Add(-1)
	}
	return n.listener.Close()
}

func TestSharedDeviceRebuildsStaleNodeWhenPlaneAnswers(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plane.Close()
	opts := sharedTestOptions(t)
	opts.ControlURL = plane.URL
	var running, started atomic.Int32
	device, err := NewSharedDevice(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { device.Close() })
	var proxies []string
	device.newNode = func(Options) (sharedNode, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		if running.Add(1) != 1 {
			t.Errorf("%d network stacks run for one device", running.Load()-1)
		}
		proxies = append(proxies, listener.Addr().String())
		n := &staleDialNode{listener: listener, closed: make(chan struct{}), running: &running}
		if started.Add(1) == 1 {
			n.fail = true
		}
		return n, nil
	}
	target := sharedEchoTarget(t)
	if err := sharedEcho(device, target); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 2 {
		t.Fatalf("started %d nodes; want 2 (stale node rebuilt)", started.Load())
	}
	if running.Load() != 1 {
		t.Fatalf("running %d network stacks after rebuild; want 1", running.Load())
	}
	if len(proxies) != 2 || proxies[0] == proxies[1] {
		t.Fatalf("proxy addresses = %v; want a republished endpoint for the rebuilt node", proxies)
	}
	endpoint, err := device.readEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Address != proxies[1] {
		t.Fatalf("published endpoint %q; want the rebuilt node's proxy %q", endpoint.Address, proxies[1])
	}
}

func TestSharedDeviceKeepsNodeWhenPlaneIsDown(t *testing.T) {
	deadPlane, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer deadPlane.Close()
	go func() {
		for {
			conn, err := deadPlane.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	opts := sharedTestOptions(t)
	opts.ControlURL = "http://" + deadPlane.Addr().String()
	var running, started atomic.Int32
	device, err := NewSharedDevice(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { device.Close() })
	var firstClosed = make(chan struct{})
	device.newNode = func(Options) (sharedNode, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		running.Add(1)
		started.Add(1)
		return &staleDialNode{listener: listener, closed: firstClosed, fail: true}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := device.Dial(ctx, "hub:9027"); err == nil || !strings.Contains(err.Error(), "stale peer state") {
		t.Fatalf("dial error = %v; want the node's stale-peer failure", err)
	}
	if started.Load() != 1 {
		t.Fatalf("started %d nodes; want 1 (no rebuild while the plane is down)", started.Load())
	}
	select {
	case <-firstClosed:
		t.Fatal("the node was closed while the plane was down")
	default:
	}
}
