package address

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"github.com/nibzard/ohya/internal/durablefile"
	"github.com/nibzard/ohya/internal/transport"
)

const (
	HubPort = 9027

	identityVersion  = 1
	hubStateDir      = "tailcat-hub"
	identityFileName = "identity.json"
	identityMaxBytes = 8192

	dialTimeout = 20 * time.Second
)

var ErrNoIdentity = errors.New("address: no saved hub identity")

type Logf = func(format string, args ...any)

type HubOptions struct {
	StateDir string

	Region *tailcfg.DERPRegion

	Logf Logf
}

type Hub struct {
	srv   *tailcat.Server
	logf  Logf
	path  string
	ident *identity
	fresh bool
	addr  string

	mu      sync.Mutex
	closed  bool
	started bool
}

var _ transport.Hub = (*Hub)(nil)

func NewHub(opts HubOptions) (*Hub, error) {
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errors.New("address: StateDir is required")
	}
	dir := filepath.Join(opts.StateDir, hubStateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("address: create hub state directory: %w", err)
	}
	path := filepath.Join(dir, identityFileName)
	ident, err := loadIdentity(path)
	if err != nil {
		return nil, err
	}
	fresh := ident == nil
	if fresh {
		if opts.Region != nil {
			if err := validateRegion(opts.Region); err != nil {
				return nil, fmt.Errorf("address: invalid Region: %w", err)
			}
		}
		ident = &identity{
			Version: identityVersion,
			Node:    key.NewNode(),
			PSK:     tailcat.NewPresharedKey(),
			Region:  opts.Region,
		}
	} else if opts.Region != nil && (ident.Region == nil || opts.Region.RegionID != ident.Region.RegionID) {
		return nil, fmt.Errorf("address: hub identity %s already pins a DERP region; restart with the same Region or without one", path)
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	srv := &tailcat.Server{
		Key:          ident.Node,
		PresharedKey: ident.PSK,
		Region:       ident.Region,
		Logf:         logf,
	}
	return &Hub{srv: srv, logf: logf, path: path, ident: ident, fresh: fresh}, nil
}

func (h *Hub) Listen(ctx context.Context) (net.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, net.ErrClosed
	}
	ln, err := h.srv.Listen(ctx, "tcp", ":"+strconv.Itoa(HubPort))
	if err != nil {
		return nil, fmt.Errorf("address: listen on tailcat port %d: %w", HubPort, err)
	}
	h.started = true
	h.addr = string(h.srv.TailcatAddr())
	if h.fresh {
		ci, err := tailcat.ParseAddr(h.srv.TailcatAddr())
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("address: parse the generated hub address: %w", err)
		}
		if len(ci.Region) == 0 {
			ln.Close()
			return nil, errors.New("address: the started hub reports no DERP region")
		}
		if h.ident.Region == nil {
			h.ident.Region = ci.Region[0]
		}
		data, err := json.Marshal(h.ident)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("address: encode hub identity: %w", err)
		}
		if err := durablefile.Write(h.path, data); err != nil {
			ln.Close()
			return nil, err
		}
		h.fresh = false
	}
	return ln, nil
}

func (h *Hub) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

func (h *Hub) PublicKey() string {
	return h.ident.Node.Public().String()
}

func (h *Hub) Region() *tailcfg.DERPRegion {
	return h.ident.Region
}

func LoadHubAddr(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("address: StateDir is required")
	}
	_, _, addr, err := loadHubForProbe(stateDir)
	return addr, err
}

func (h *Hub) Drain(ctx context.Context) error {
	h.mu.Lock()
	started := h.started
	h.mu.Unlock()
	if !started {
		return nil
	}
	return h.srv.DrainTCP(ctx)
}

func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if err := h.srv.Close(); err != nil {
		return fmt.Errorf("address: close hub: %w", err)
	}
	return nil
}

type DeviceOptions struct {
	Logf Logf
}

type Device struct {
	client *tailcat.Client
	logf   Logf

	mu      sync.Mutex
	addr    tailcat.Addr
	closed  bool
	started bool
}

var _ transport.Device = (*Device)(nil)

func NewDevice(opts DeviceOptions) *Device {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Device{
		client: &tailcat.Client{Key: key.NewNode(), Logf: logf},
		logf:   logf,
	}
}

func (d *Device) Dial(ctx context.Context, addr string) (net.Conn, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, net.ErrClosed
	}
	if d.addr != "" && d.addr != tailcat.Addr(addr) {
		d.mu.Unlock()
		return nil, errors.New("address: this device already dials another hub")
	}
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("address: invalid hub address: %w", err)
	}
	if d.addr == "" {
		d.addr = tailcat.Addr(addr)
		d.client.Server = d.addr
	}
	client := d.client
	d.mu.Unlock()

	dialCtx := ctx
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > dialTimeout {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	if _, err := client.Ping(dialCtx); err != nil {
		d.resetClient(client)
		return nil, fmt.Errorf("address: reach the hub through its relay: %w", err)
	}
	conn, err := client.DialTCPPort(dialCtx, HubPort)
	if err != nil {
		d.resetClient(client)
		return nil, fmt.Errorf("address: dial the hub on port %d: %w", HubPort, err)
	}
	d.mu.Lock()
	if d.client == client {
		d.started = true
	}
	d.mu.Unlock()
	return conn, nil
}

func (d *Device) resetClient(old *tailcat.Client) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.client != old {
		return
	}
	_ = old.Close()
	fresh := &tailcat.Client{Key: key.NewNode(), Logf: d.logf}
	fresh.Server = d.addr
	d.client = fresh
	d.started = false
}

func (d *Device) Drain(ctx context.Context) error {
	d.mu.Lock()
	client := d.client
	started := d.started
	d.mu.Unlock()
	if !started {
		return nil
	}
	return client.DrainTCP(ctx)
}

func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.client.Close()
}

type identity struct {
	Version int                  `json:"version"`
	Node    key.NodePrivate      `json:"node"`
	PSK     tailcat.PresharedKey `json:"preshared_key"`
	Region  *tailcfg.DERPRegion  `json:"region"`
}

type identityJSON struct {
	Version int             `json:"version"`
	Node    string          `json:"node"`
	PSK     string          `json:"preshared_key"`
	Region  json.RawMessage `json:"region"`
}

func loadIdentity(path string) (*identity, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("address: stat hub identity: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("address: hub identity %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("address: hub identity %s must be an owner-only file, mode %o", path, info.Mode().Perm())
	}
	if info.Size() > identityMaxBytes {
		return nil, fmt.Errorf("address: hub identity %s is %d bytes, over the %d byte limit", path, info.Size(), identityMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("address: read hub identity: %w", err)
	}
	var raw identityJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("address: hub identity %s is not valid JSON: %w", path, err)
	}
	if raw.Version != identityVersion {
		return nil, fmt.Errorf("address: hub identity %s has version %d, want %d", path, raw.Version, identityVersion)
	}
	var id identity
	id.Version = raw.Version
	if err := id.Node.UnmarshalText([]byte(raw.Node)); err != nil {
		return nil, fmt.Errorf("address: hub identity %s has an invalid node key: %w", path, err)
	}
	if err := id.PSK.UnmarshalText([]byte(raw.PSK)); err != nil {
		return nil, fmt.Errorf("address: hub identity %s has an invalid pre-shared key", path)
	}
	if len(raw.Region) == 0 || string(raw.Region) == "null" {
		return nil, fmt.Errorf("address: hub identity %s has no DERP region", path)
	}
	var region tailcfg.DERPRegion
	if err := json.Unmarshal(raw.Region, &region); err != nil {
		return nil, fmt.Errorf("address: hub identity %s has an invalid DERP region: %w", path, err)
	}
	id.Region = &region
	if id.Node.IsZero() || id.PSK.IsZero() {
		return nil, fmt.Errorf("address: hub identity %s has zero key material", path)
	}
	if err := validateRegion(id.Region); err != nil {
		return nil, fmt.Errorf("address: hub identity %s: %w", path, err)
	}
	return &id, nil
}

func validateRegion(r *tailcfg.DERPRegion) error {
	if r == nil {
		return errors.New("DERP region is required")
	}
	if r.RegionID == 0 {
		return errors.New("DERP region has no region ID")
	}
	relays := 0
	for _, n := range r.Nodes {
		if n == nil {
			return errors.New("DERP region has a null node")
		}
		if n.STUNOnly {
			continue
		}
		if n.HostName == "" {
			return errors.New("DERP region has a relay node without a host name")
		}
		relays++
	}
	if relays == 0 {
		return errors.New("DERP region has no relay nodes")
	}
	return nil
}
