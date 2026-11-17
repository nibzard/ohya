package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBootstrapNoStableReleaseRedirect(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("shell bootstrap targets Unix")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases", http.StatusFound)
		case "/releases":
			_, _ = w.Write([]byte("fixture release list"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := server.URL + "/releases"
	output, err := runInstaller(ctx, t, script, ca, nil,
		[]string{"OHYA_RELEASE_ROOT=" + root}, "--no-agents", "--no-service")
	if err == nil {
		t.Fatalf("accepted a repository without a stable release: %s", output)
	}
	if !strings.Contains(output, "no stable release") {
		t.Fatalf("output = %s; want the no-stable-release diagnostic", output)
	}
	if !strings.Contains(output, root) {
		t.Fatalf("output = %s; want the release list location", output)
	}
	if !strings.Contains(output, "--version") {
		t.Fatalf("output = %s; want the --version guidance", output)
	}
	if strings.Contains(output, "must start with v and a digit") {
		t.Fatalf("output = %s; the generic version diagnostic replaced the redirect diagnostic", output)
	}
}

func TestBootstrapVerifiedRelease(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("shell bootstrap targets Unix")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	for _, test := range []struct {
		name        string
		badChecksum bool
		badEntry    bool
	}{{"valid", false, false}, {"corrupt", true, false}, {"traversal", false, true}} {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			compressed := gzip.NewWriter(&archive)
			tw := tar.NewWriter(compressed)
			for _, name := range []string{"INSTALL.md", "LICENSE", "ohya"} {
				data := []byte("fixture")
				if name == "ohya" {
					data = []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'ohya version=v9.8.7'; else printf '%s\\n' \"$@\"; fi\n")
				}
				if test.badEntry && name == "ohya" {
					name = "../ohya"
				}
				if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(data))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			sum := fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
			if test.badChecksum {
				sum = strings.Repeat("0", 64)
			}
			asset := fmt.Sprintf("ohya_v9.8.7_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/latest":
					http.Redirect(w, r, "/tag/v9.8.7", http.StatusFound)
				case "/tag/v9.8.7":
					_, _ = w.Write([]byte("fixture release"))
				case "/download/v9.8.7/SHA256SUMS":
					fmt.Fprintf(w, "%s  %s\n", sum, asset)
				case "/download/v9.8.7/" + asset:
					_, _ = w.Write(archive.Bytes())
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			ca := filepath.Join(dir, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			script, err := filepath.Abs("../../install.sh")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", script, "--bin-dir", filepath.Join(dir, "space in bin"), "--no-agents", "--no-service")
			cmd.Env = append(os.Environ(), "OHYA_RELEASE_ROOT="+server.URL, "CURL_CA_BUNDLE="+ca, "TMPDIR="+dir)
			output, err := cmd.CombinedOutput()
			if test.badChecksum || test.badEntry {
				if err == nil {
					t.Fatalf("accepted invalid release: %s", output)
				}
				if bytes.Contains(output, []byte("install\n")) {
					t.Fatal("ran installer before verification")
				}
			} else if err != nil || !bytes.Contains(output, []byte("install\n")) {
				t.Fatalf("bootstrap: %v %s", err, output)
			}
		})
	}
}
