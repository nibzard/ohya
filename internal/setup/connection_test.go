package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConnectionChoiceAndModeBinding(t *testing.T) {
	state := t.TempDir()
	c, err := LoadConnection(state)
	if err != nil || c.Mode != "local" {
		t.Fatalf("default %+v %v", c, err)
	}
	remote := Connection{Mode: "remote"}
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
	if err := BindRemote(join, Connection{Mode: "local"}); err == nil {
		t.Fatal("rebound a remote join state to local mode")
	}
	if err := SaveConnection(state, Connection{Mode: "local"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(state, "setup", "connection.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("connection permissions %v %v", info, err)
	}
}

func TestConnectionRejectsInvalidModes(t *testing.T) {
	for _, c := range []Connection{{Mode: "unknown"}, {Mode: ""}, {Mode: "Remote"}} {
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestConnectionIgnoresRetiredFields(t *testing.T) {
	state := t.TempDir()
	legacy := []byte(`{"mode":"remote","control_url":"https://hub.example","hub_address":"ohya-hub.tail.ohya:9027"}`)
	if err := os.MkdirAll(filepath.Join(state, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "setup", "connection.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConnection(state)
	if err != nil || c.Mode != "remote" {
		t.Fatalf("legacy connection = %+v, %v; want remote mode", c, err)
	}
	if err := BindRemote(filepath.Join(state, "join"), Connection{Mode: "remote"}); err != nil {
		t.Fatalf("rerun over a legacy connection failed: %v", err)
	}
}
