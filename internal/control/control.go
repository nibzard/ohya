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

const DirName = "control"

const UserName = "ohya"

const HubNodeName = "ohya-hub"

var ErrNotProvisioned = errors.New("control: holds no coordination plane")

const (
	DefaultListenAddr = "127.0.0.1:9417"
	DefaultSTUNAddr   = "127.0.0.1:3478"
)

func DefaultURL() string { return "http://" + DefaultListenAddr }

type Options struct {
	Dir        string
	ServerURL  string
	ListenAddr string
	STUNAddr   string
	BaseDomain string
	LogLevel   string
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

const configFileName = "config.yaml"

type Control struct {
	cfg  *types.Config
	dir  string
	app  *hscontrol.Headscale
	st   *state.State
	boot chan error
}

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

func Open(dir string) (*Control, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("control: resolve %s: %w", dir, err)
	}
	path := filepath.Join(abs, configFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s; run ohya serve once to provision it", ErrNotProvisioned, abs)
	} else if err != nil {
		return nil, fmt.Errorf("control: stat %s: %w", path, err)
	}
	return load(path)
}

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

func (c *Control) URL() string { return c.cfg.ServerURL }

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

func (c *Control) Close() error {
	if c.st == nil {
		return nil
	}
	err := c.st.Close()
	c.st = nil
	return err
}

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

func MintAuthKey(st *state.State, user types.User, ttl time.Duration) (string, error) {
	exp := time.Now().Add(ttl)
	uid := types.UserID(user.ID)
	key, err := st.CreatePreAuthKey(&uid, false, false, &exp, nil)
	if err != nil {
		return "", fmt.Errorf("control: mint preauth key: %w", err)
	}
	return key.Key, nil
}

func HubEnrolled(st *state.State) bool {
	_, ok := st.ResolveNode(HubNodeName)
	return ok
}

func SplitJoinCode(code string) (invitation, authKey string, err error) {
	parts := strings.Split(code, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("control: join code must be <invitation>:<preauth-key> as printed by ohya devices invite")
	}
	return parts[0], parts[1], nil
}
