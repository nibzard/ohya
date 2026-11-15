package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicBinaryLifecycle(t *testing.T) {
	root := t.TempDir()
	source, dir := filepath.Join(root, "source"), filepath.Join(root, "bin with spaces")
	if err := os.WriteFile(source, []byte("first"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := Binary(context.Background(), source, dir)
	if err != nil || !r.Changed {
		t.Fatalf("install: %+v %v", r, err)
	}
	r, err = Binary(context.Background(), source, dir)
	if err != nil || r.Changed {
		t.Fatalf("repeat: %+v %v", r, err)
	}
	if err := os.WriteFile(source, []byte("next"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err = Binary(context.Background(), source, dir)
	if err != nil || !r.Changed || r.Recovery == "" {
		t.Fatalf("upgrade: %+v %v", r, err)
	}
	previous, err := os.ReadFile(r.Recovery)
	if err != nil || string(previous) != "first" {
		t.Fatalf("recovery: %q %v", previous, err)
	}
	if err := os.WriteFile(r.Path, []byte("user edit"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Binary(context.Background(), source, dir); err == nil {
		t.Fatal("overwrote edited executable")
	}
}

func TestBinaryPreservesForeignDestinationAndSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("source"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, link := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "ohya")
		if link {
			if err := os.Symlink(source, path); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(path, []byte("foreign"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Binary(context.Background(), source, dir); err == nil {
			t.Fatal("overwrote foreign destination")
		}
	}
}
