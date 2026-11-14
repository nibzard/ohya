// Package control provisions and runs the self-hosted coordination plane of
// an ohya hub: one embedded Headscale control server with its own SQLite
// database, a generated configuration, and the embedded DERP relay, all
// managed inside the serve command (the control-plane decision recorded in
// ADR 0002). The pinned dependency — headscale v0.29.7, the tested pair with
// the pinned tailscale client library of internal/transport/tailcat — stays
// isolated here, exactly the way the tsnet client stays inside the transport
// adapter. Pairing invitations stay in the T018 credential registry; this
// package only owns coordination: nodes, the single-use preauth keys that
// bridge T018 invitations into tailnet admission, and relayed connectivity.
//
// The coordination state lives in its own directory (DirName) beside the hub
// database and the registry: a generated config.yaml, one SQLite database,
// the noise and DERP keys, and the headscale gRPC unix socket. A second
// process sharing the directory — for example `ohya devices invite` — opens
// the same database through the same pinned library, and the driver's busy
// timeout serializes the rare concurrent write.
package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	hscontrol "github.com/juanfont/headscale/hscontrol"
	"github.com/juanfont/headscale/hscontrol/state"
	"github.com/juanfont/headscale/hscontrol/types"
)

// DirName is the coordination state directory inside the hub state directory.
const DirName = "control"

// UserName is the single coordinator user every ohya node enrolls under.
// Coordination admission is per preauth key, not per user; authorization of
// devices is the credential registry's job (internal/registry, tla/Auth.tla).
const UserName = "ohya"

// HubNodeName is the tailnet hostname of the hub's own transport node.
// Devices dial it inside the tailnet; it must stay stable across runs.
const HubNodeName = "ohya-hub"

// Default listen addresses of the coordination plane. The control listener
// carries the coordination protocol and the embedded DERP relay; the STUN
// listener helps devices behind NAT discover their endpoint. Both default to
// loopback, which serves the local-first single-machine deployment; a hub
// reachable from other machines overrides both plus the advertised URL.
const (
	DefaultListenAddr = "127.0.0.1:9417"
	DefaultSTUNAddr   = "127.0.0.1:3478"
)

// DefaultURL is the coordination URL devices use when the hub listens on the
// default loopback address.
func DefaultURL() string { return "http://" + DefaultListenAddr }

// Options configures one coordination plane. Every field is required unless
// its comment says otherwise.
type Options struct {
	// Dir is the coordination state directory; created with owner-only
	// permissions.
	Dir string
	// ServerURL is the URL devices reach the coordination plane at. It must
	// match ListenAddr's host and port unless a proxy sits in between.
	ServerURL string
	// ListenAddr is the host:port the control server binds.
	ListenAddr string
	// STUNAddr is the UDP host:port the embedded relay's STUN helper binds.
	STUNAddr string
	// BaseDomain is the MagicDNS domain of the tailnet; the hub's dialable
	// name is HubNodeName + "." + BaseDomain.
	BaseDomain string
	// LogLevel is the control server's zerolog level. The default is "warn";
	// "info" or "debug" exist for diagnosing connectivity. Optional.
	LogLevel string
}

func (o Options) validate() error {
	if o.Dir == "" {
		return fmt.Errorf("control: Dir is required")
	}
	if o.ServerURL == "" {
		return fmt.Errorf("control: ServerURL is required")
	}
	if u, err := url.Parse(o.ServerURL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("control: ServerURL %q is not an absolute URL", o.ServerURL)
	}
	if o.ListenAddr == "" {
		return fmt.Errorf("control: ListenAddr is required")
	}
	if o.STUNAddr == "" {
		return fmt.Errorf("control: STUNAddr is required")
	}
	if o.BaseDomain == "" {
		return fmt.Errorf("control: BaseDomain is required")
	}
	return nil
}

// configFileName is the generated headscale configuration inside Dir.
const configFileName = "config.yaml"

// Control is one coordination plane: its loaded configuration, the running
// server, and an on-demand state handle for administration (users, preauth
// keys). Build one with Provision or Open; it is not safe for concurrent use.
type Control struct {
	cfg  *types.Config
	dir  string
	app  *hscontrol.Headscale
	st   *state.State
	boot chan error // Serve's terminal error, buffered
}

// Provision writes the generated configuration into Dir and loads it. The
// file is regenerated from the options on every call: the options are the
// source of truth, and equal options produce an equal file, so an unchanged
// restart rewrites nothing new. A hand-edited file is overwritten — advanced
// headscale settings do not belong in ohya's generated plane.
func Provision(opts Options) (*Control, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("control: resolve %s: %w", opts.Dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("control: create %s: %w", dir, err)
	}
	logLevel := opts.LogLevel
	if logLevel == "" {
		logLevel = "warn"
	}
	// Absolute paths only: headscale resolves relative paths against the
	// config file's directory, which would nest the state directory twice.
	cfgYAML := fmt.Sprintf(`server_url: %s
listen_addr: %s
metrics_listen_addr: ""
grpc_listen_addr: 127.0.0.1:0
grpc_allow_insecure: false
unix_socket: %s
unix_socket_permission: 0o700
noise:
  private_key_path: %s
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    enabled: true
    region_id: 999
    region_code: ohya
    region_name: ohya embedded DERP
    verify_clients: true
    stun_listen_addr: %s
    private_key_path: %s
    automatically_add_embedded_derp_region: true
  # No external DERP maps: the local-first plane runs no third-party service
  # in the control or data path (MANIFESTO.md).
  urls: []
  paths: []
  auto_update_enabled: false
dns:
  magic_dns: true
  base_domain: %s
  override_local_dns: false
  nameservers:
    global: []
database:
  type: sqlite
  sqlite:
    path: %s
log:
  level: %s
`,
		opts.ServerURL, opts.ListenAddr,
		filepath.Join(dir, "headscale.sock"),
		filepath.Join(dir, "noise.key"),
		opts.STUNAddr,
		filepath.Join(dir, "derp.key"),
		opts.BaseDomain,
		filepath.Join(dir, "db.sqlite"),
		logLevel,
	)
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		return nil, fmt.Errorf("control: write %s: %w", path, err)
	}
	return load(path)
}

// Open loads the existing generated configuration of a previously provisioned
// coordination plane. It does not rewrite the file; `ohya devices invite`
// uses this to mint preauth keys against a hub that served at least once.
func Open(dir string) (*Control, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("control: resolve %s: %w", dir, err)
	}
	path := filepath.Join(abs, configFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("control: %s holds no coordination plane; run ohya serve once to provision it", abs)
	} else if err != nil {
		return nil, fmt.Errorf("control: stat %s: %w", path, err)
	}
	return load(path)
}

// load reads the configuration through headscale's own loader, so defaults
// and validation behave exactly as the pinned library expects.
func load(path string) (*Control, error) {
	if err := types.LoadConfig(path, true); err != nil {
		return nil, fmt.Errorf("control: load %s: %w", path, err)
	}
	cfg, err := types.LoadServerConfig()
	if err != nil {
		return nil, fmt.Errorf("control: load %s: %w", path, err)
	}
	return &Control{cfg: cfg, dir: filepath.Dir(path)}, nil
}

// Start boots the control server in-process and returns once it answers its
// health endpoint, so the hub node can enroll immediately after. The server
// runs until the process exits or receives a terminating signal, which its
// own handler shuts down gracefully; a listener that cannot bind — a second
// serve on the same state, for example — fails here with its reason.
func (c *Control) Start(ctx context.Context) error {
	app, err := hscontrol.NewHeadscale(c.cfg)
	if err != nil {
		return fmt.Errorf("control: build server: %w", err)
	}
	c.app = app
	c.boot = make(chan error, 1)
	go func() { c.boot <- app.Serve() }()
	return c.waitHealthy(ctx)
}

// waitHealthy polls the health endpoint until the server answers, watching
// for an early Serve failure that carries the real reason.
func (c *Control) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.cfg.ServerURL+"/health", nil)
		if err != nil {
			return fmt.Errorf("control: health request: %w", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case err := <-c.boot:
			return fmt.Errorf("control: server stopped: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control: server never became healthy: %w", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// URL reports the coordination URL devices dial.
func (c *Control) URL() string { return c.cfg.ServerURL }

// State opens the administration handle of the coordination database: users
// and preauth keys (EnsureUser, MintAuthKey). The handle stays open until
// Close; callers never close it themselves.
func (c *Control) State() (*state.State, error) {
	if c.st != nil {
		return c.st, nil
	}
	st, err := state.NewState(c.cfg)
	if err != nil {
		return nil, fmt.Errorf("control: open coordination state: %w", err)
	}
	c.st = st
	return st, nil
}

// Close releases the administration handle. The embedded server itself has
// no in-process stop; it ends with the process.
func (c *Control) Close() error {
	if c.st == nil {
		return nil
	}
	err := c.st.Close()
	c.st = nil
	return err
}

// EnsureUser returns the coordinator user ohya nodes enroll under, creating
// it on first use.
func EnsureUser(st *state.State) (types.User, error) {
	user, err := st.GetUserByName(UserName)
	if err == nil {
		return *user, nil
	}
	created, _, cerr := st.CreateUser(types.User{Name: UserName})
	if cerr != nil {
		return types.User{}, fmt.Errorf("control: create user %s: %w (lookup: %v)", UserName, cerr, err)
	}
	return *created, nil
}

// MintAuthKey creates one single-use preauth key under user, valid for ttl:
// the coordination-side half of a pairing invitation. Each key admits one
// node exactly once, mirroring the single-use invitation of tla/Auth.tla at
// the transport layer.
func MintAuthKey(st *state.State, user types.User, ttl time.Duration) (string, error) {
	exp := time.Now().Add(ttl)
	uid := types.UserID(user.ID)
	key, err := st.CreatePreAuthKey(&uid, false, false, &exp, nil)
	if err != nil {
		return "", fmt.Errorf("control: mint preauth key: %w", err)
	}
	return key.Key, nil
}

// HubEnrolled reports whether the hub's own node has already enrolled, so a
// restart mints no fresh admission key for it (a durable node reuses its
// identity and consumes no key).
func HubEnrolled(st *state.State) bool {
	_, ok := st.ResolveNode(HubNodeName)
	return ok
}

// SplitJoinCode splits a join code into its two single-use halves: the T018
// pairing invitation and the coordinator preauth key minted with it. The
// operator hands one code to one device; each half admits exactly one use.
func SplitJoinCode(code string) (invitation, authKey string, err error) {
	parts := strings.Split(code, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("control: join code must be <invitation>:<preauth-key> as printed by ohya devices invite")
	}
	return parts[0], parts[1], nil
}
