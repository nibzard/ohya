package setup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

func TestLegacyPiSetupAndNativeTransition(t *testing.T) {
	o := engineOptions(t)
	o.Clients.Agents = "pi"
	o.ProbePi = func(context.Context, string) error { return ErrPiNeedsExtension }
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("legacy setup %+v %v", report, err)
	}
	extension := filepath.Join(o.Clients.Home, ".pi", "agent", "extensions", "ohya-memory.js")
	if _, err := os.Stat(extension); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.Clients.Home, ".pi", "agent", "mcp.json")); !os.IsNotExist(err) {
		t.Fatal("legacy setup wrote native config")
	}
	o.ProbePi = func(context.Context, string) error { return nil }
	report, err = Run(context.Background(), o)
	if err != nil || !report.Complete() {
		t.Fatalf("native transition %+v %v", report, err)
	}
	if _, err := os.Stat(extension); !os.IsNotExist(err) {
		t.Fatal("legacy extension remains with native MCP")
	}
}

func TestPiExtensionThroughInstalledLoader(t *testing.T) {
	packageRoot := os.Getenv("OHYA_PI_PACKAGE")
	binary := os.Getenv("OHYA_TEST_BINARY")
	if packageRoot == "" || binary == "" {
		t.Skip("set OHYA_PI_PACKAGE and OHYA_TEST_BINARY for native Pi verification")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	device := strings.Repeat("a", 32)
	if err := hub.RegisterSource(context.Background(), "test", device, "pi", "/synthetic.jsonl"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Ingest(context.Background(), store.Batch{SourceID: "test", DeviceID: device, Events: []store.Event{{Offset: 0, End: 3, NativeID: "test", Kind: "message", RawJSON: "{}", ParserVersion: 1}}}); err != nil {
		t.Fatal(err)
	}
	o := ClientOptions{Home: dir, State: filepath.Join(dir, "state"), Binary: binary, Args: []string{"mcp", "--db", dbPath, "--state", filepath.Join(dir, "state")}}
	c := Client{Name: "pi", Config: filepath.Join(dir, "agent", "mcp.json")}
	if _, err := ApplyPiExtension(o, c); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "verify.mjs")
	const verification = `import { pathToFileURL } from "node:url";
import { join, dirname } from "node:path";
const { DefaultResourceLoader } = await import(pathToFileURL(join(process.argv[2], "dist/core/resource-loader.js")));
const loader = new DefaultResourceLoader({ cwd: process.cwd(), agentDir: dirname(dirname(process.argv[3])) });
await loader.reload();
const loaded = loader.getExtensions();
if (loaded.errors.length) throw new Error(JSON.stringify(loaded.errors));
const tools = loaded.extensions.find(extension => extension.tools.has("ohya_sql")).tools;
const sql = tools.get("ohya_sql").definition;
const annotate = tools.get("ohya_annotate").definition;
const first = await sql.execute("check", { query: "SELECT id FROM events LIMIT 1" });
const id = first.details.rows[0][0];
await annotate.execute("annotate", { operations: [{ op: "upsert", event_id: id, namespace: "setup_test", key: "check", value: {text: "记忆 🧠"} }] });
const read = await sql.execute("read", { query: "SELECT json_extract(value, '$.text') FROM event_annotations WHERE namespace = 'setup_test'" });
if (read.details.rows[0][0] !== "记忆 🧠") throw new Error("annotation round trip failed");
console.log("Pi loader, SQL, and annotation round trip passed");
`
	if err := os.WriteFile(script, []byte(verification), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script, packageRoot, filepath.Join(dir, "agent", "extensions", "ohya-memory.js"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+filepath.Join(dir, "agent"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Pi verification: %v %s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
	if _, err := RemovePiExtension(o.State); err != nil {
		t.Fatal(err)
	}
}
