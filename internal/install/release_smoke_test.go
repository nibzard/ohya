package install

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
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

func TestBootstrapRealReleaseArchive(t *testing.T) {
	dir := os.Getenv("OHYA_TEST_RELEASE_DIR")
	if dir == "" {
		t.Skip("set OHYA_TEST_RELEASE_DIR to test a native packaged candidate")
	}
	suffix := "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	matches, err := filepath.Glob(filepath.Join(dir, "ohya_*"+suffix))
	if err != nil || len(matches) != 1 {
		t.Fatalf("need one native archive: %v %v", matches, err)
	}
	asset := filepath.Base(matches[0])
	version := strings.TrimSuffix(strings.TrimPrefix(asset, "ohya_"), suffix)
	manifest, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS."+runtime.GOOS+"-"+runtime.GOARCH))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + version + "/" + asset:
			http.ServeFile(w, r, matches[0])
		case "/download/" + version + "/SHA256SUMS":
			_, _ = w.Write(manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	work := t.TempDir()
	ca := filepath.Join(work, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script, "--bin-dir", filepath.Join(work, "bin with spaces"), "--state", filepath.Join(work, "state"), "--no-agents", "--no-service", "--no-modify-path", "--json")
	cmd.Env = append(os.Environ(), "OHYA_RELEASE_ROOT="+server.URL, "CURL_CA_BUNDLE="+ca, "TMPDIR="+work)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("real bootstrap: %v %s %s", err, stdout.String(), stderr.String())
	}
	var result struct {
		Command string `json:"command"`
		Error   any    `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Command != "ohya setup" || result.Error != nil {
		t.Fatalf("AX result %s %v", stdout.String(), err)
	}
	if _, err := os.Stat(filepath.Join(work, "bin with spaces", "ohya")); err != nil {
		t.Fatal(err)
	}
}
