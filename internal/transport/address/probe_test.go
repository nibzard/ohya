package address

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func startStoppableDERP(t *testing.T) (*tailcfg.DERPRegion, func()) {
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
	port := httpsrv.Listener.Addr().(*net.TCPAddr).Port
	region := &tailcfg.DERPRegion{
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
	stop := func() {
		httpsrv.CloseClientConnections()
		httpsrv.Close()
		srv.Close()
	}
	t.Cleanup(stop)
	return region, stop
}

func startProbedHub(t *testing.T, stateDir string, region *tailcfg.DERPRegion) *Hub {
	t.Helper()
	hub, err := NewHub(HubOptions{StateDir: stateDir, Region: region})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	return hub
}

func TestProbeHubAnswersThroughRelay(t *testing.T) {
	stateDir := t.TempDir()
	region, _ := startStoppableDERP(t)
	hub := startProbedHub(t, stateDir, region)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ln, err := hub.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer probeCancel()
	info, err := ProbeHub(probeCtx, stateDir)
	if err != nil {
		t.Fatalf("probe = %v (info %+v)", err, info)
	}
	if info.NodeKey == "" || info.RegionCode != region.RegionCode {
		t.Fatalf("info = %+v", info)
	}
	if info, err := LoadHubInfo(stateDir); err != nil || info.NodeKey == "" {
		t.Fatalf("LoadHubInfo = %+v %v", info, err)
	}
}

func TestProbeHubNamesUnreachableRelay(t *testing.T) {
	stateDir := t.TempDir()
	region, stop := startStoppableDERP(t)
	hub := startProbedHub(t, stateDir, region)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ln, err := hub.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	stop()

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer probeCancel()
	_, err = ProbeHub(probeCtx, stateDir)
	if !errors.Is(err, ErrRelayUnreachable) {
		t.Fatalf("err = %v; want ErrRelayUnreachable", err)
	}
}

func TestProbeHubNamesSilentHub(t *testing.T) {
	stateDir := t.TempDir()
	region, _ := startStoppableDERP(t)
	hub := startProbedHub(t, stateDir, region)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ln, err := hub.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer probeCancel()
	_, err = ProbeHub(probeCtx, stateDir)
	if !errors.Is(err, ErrHubUnreachable) {
		t.Fatalf("err = %v; want ErrHubUnreachable", err)
	}
}

func TestRelayEndpointDefaultsToHTTPS(t *testing.T) {
	relay, err := relayEndpoint(&tailcfg.DERPRegion{Nodes: []*tailcfg.DERPNode{{HostName: "relay.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	if relay.host != "relay.example" || relay.port != 443 {
		t.Fatalf("relay = %+v", relay)
	}
	stunOnly := &tailcfg.DERPRegion{Nodes: []*tailcfg.DERPNode{{HostName: "stun.example", STUNOnly: true}}}
	if _, err := relayEndpoint(stunOnly); err == nil {
		t.Fatal("accepted a region without relay nodes")
	}
}
