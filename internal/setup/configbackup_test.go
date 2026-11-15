package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestClientConfigurationBackups(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			o := engineOptions(t).Clients
			c := Client{Name: name, Config: filepath.Join(o.Home, name+".config")}
			original := []byte("# preserve formatting\nmodel = 'keep'\n")
			if name == "claude" {
				original = []byte("{\n  \"model\": \"keep\"\n}\n")
			}
			if err := os.WriteFile(c.Config, original, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyClient(o, c); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(o.State, "setup", "clients", name+".json.backups")
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("backups %v %v", files, err)
			}
			saved := filepath.Join(dir, files[0].Name())
			raw, err := os.ReadFile(saved)
			if err != nil || !bytes.Equal(raw, original) {
				t.Fatalf("backup %q %v", raw, err)
			}
			for _, path := range []string{dir, saved} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm()&0077 != 0 {
					t.Fatalf("private backup %s %v", path, err)
				}
			}
			if changed, err := ApplyClient(o, c); err != nil || changed {
				t.Fatalf("repeat %v %v", changed, err)
			}
			files, _ = os.ReadDir(dir)
			if len(files) != 1 {
				t.Fatal("duplicate backup on no-op")
			}
			if _, err := RemoveClient(o.State, name); err != nil {
				t.Fatal(err)
			}
			raw, err = os.ReadFile(saved)
			if err != nil || !bytes.Equal(raw, original) {
				t.Fatal("uninstall removed original backup")
			}
		})
	}
}
