package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConnectionChoiceAndHubBinding(t *testing.T) {
	state := t.TempDir()
	c, err := LoadConnection(state)
	if err != nil || c.Mode != "local" {
		t.Fatalf("default %+v %v", c, err)
	}
	remote := Connection{Mode: "remote", ControlURL: "https://hub.example", HubAddress: "ohya-hub.tail.ohya:9027"}
	if err := SaveConnection(state, remote); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConnection(state)
	if err != nil || c != remote {
		t.Fatalf("saved %+v %v", c, err)
	}
	join := filepath.Join(state, "join")
	if err := BindRemote(join, remote); err != nil {
		t.Fatal(err)
	}
	if err := BindRemote(join, remote); err != nil {
		t.Fatal(err)
	}
	other := remote
	other.ControlURL = "https://other.example"
	if err := BindRemote(join, other); err == nil {
		t.Fatal("rebound identity to another hub")
	}
	if err := SaveConnection(state, Connection{Mode: "local"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(state, "setup", "connection.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("connection permissions %v %v", info, err)
	}
}

func TestConnectionRejectsCredentialsAndInvalidModes(t *testing.T) {
	for _, c := range []Connection{{Mode: "unknown"}, {Mode: "local", ControlURL: "https://hub"}, {Mode: "remote", ControlURL: "https://user:secret@hub", HubAddress: "hub:1"}, {Mode: "remote", ControlURL: "https://hub?secret=token", HubAddress: "hub:1"}, {Mode: "remote", ControlURL: "https://hub", HubAddress: "missing-port"}} {
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
