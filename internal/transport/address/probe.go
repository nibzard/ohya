package address

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

var (
	ErrRelayUnreachable = errors.New("address: no relay is reachable")
	ErrHubUnreachable   = errors.New("address: the hub does not answer through its relay")
)

type HubInfo struct {
	NodeKey    string
	RegionID   int64
	RegionCode string
	RegionName string
}

func LoadHubInfo(stateDir string) (HubInfo, error) {
	info, _, _, err := loadHubForProbe(stateDir)
	return info, err
}

func ProbeHub(ctx context.Context, stateDir string) (HubInfo, error) {
	info, relay, hubAddr, err := loadHubForProbe(stateDir)
	if err != nil {
		return info, err
	}
	if err := dialRelay(ctx, relay); err != nil {
		return info, err
	}
	node := NewDevice(DeviceOptions{})
	defer node.Close()
	conn, err := node.Dial(ctx, hubAddr)
	if err != nil {
		return info, fmt.Errorf("%w: %w", ErrHubUnreachable, err)
	}
	return info, conn.Close()
}

type hostPort struct {
	host string
	port int
}

func loadHubForProbe(stateDir string) (HubInfo, hostPort, string, error) {
	ident, err := loadIdentity(filepath.Join(stateDir, hubStateDir, identityFileName))
	if err != nil {
		return HubInfo{}, hostPort{}, "", err
	}
	if ident == nil {
		return HubInfo{}, hostPort{}, "", fmt.Errorf("%w at %s; run ohya serve once to create it", ErrNoIdentity, filepath.Join(stateDir, hubStateDir, identityFileName))
	}
	relay, err := relayEndpoint(ident.Region)
	if err != nil {
		return HubInfo{}, hostPort{}, "", err
	}
	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: ident.Node.Public()},
		ServerDiscoPublic: tailcat.DiscoPublicForNode(ident.Node),
		PresharedKey:      ident.PSK,
		Region:            []*tailcfg.DERPRegion{ident.Region},
	}
	return HubInfo{
		NodeKey:    ident.Node.Public().String(),
		RegionID:   int64(ident.Region.RegionID),
		RegionCode: ident.Region.RegionCode,
		RegionName: ident.Region.RegionName,
	}, relay, string(ci.Addr()), nil
}

func relayEndpoint(region *tailcfg.DERPRegion) (hostPort, error) {
	for _, n := range region.Nodes {
		if n == nil || n.STUNOnly {
			continue
		}
		port := n.DERPPort
		if port == 0 {
			port = 443
		}
		return hostPort{host: n.HostName, port: port}, nil
	}
	return hostPort{}, errors.New("address: the hub identity names no relay node")
}

func dialRelay(ctx context.Context, relay hostPort) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(relay.host, strconv.Itoa(relay.port)))
	if err != nil {
		return fmt.Errorf("%w: %s needs outbound HTTPS (TCP %d): %w", ErrRelayUnreachable, relay.host, relay.port, err)
	}
	return conn.Close()
}
