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

func TestRemovalRespectsExecutableOwnership(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	dir := filepath.Join(root, "bin")
	if err := os.WriteFile(source, []byte("installed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Binary(context.Background(), source, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ohya"), []byte("edited"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(context.Background(), dir); err == nil {
		t.Fatal("removed edited executable")
	}
	if err := os.WriteFile(filepath.Join(dir, "ohya"), []byte("installed"), 0700); err != nil {
		t.Fatal(err)
	}
	changed, err := Remove(context.Background(), dir)
	if err != nil || !changed {
		t.Fatalf("remove %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ohya")); !os.IsNotExist(err) {
		t.Fatal("executable remains")
	}
	changed, err = Remove(context.Background(), dir)
	if err != nil || changed {
		t.Fatalf("repeat removal %v %v", changed, err)
	}
	unmanaged := t.TempDir()
	if err := os.WriteFile(filepath.Join(unmanaged, "ohya"), []byte("manual"), 0700); err != nil {
		t.Fatal(err)
	}
	if changed, err := Remove(context.Background(), unmanaged); err != nil || changed {
		t.Fatalf("changed manual executable: %v %v", changed, err)
	}
}
