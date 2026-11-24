package secrets

import (
	"fmt"
	"strings"
	"testing"
)

func repeat(s string, n int) string { return strings.Repeat(s, n) }

func pemBlock(label, body string) string {
	return "-----BEGIN " + label + "-----\n" + body + "\n-----END " + label + "-----"
}

func TestRedactMatchesRealShapedSecrets(t *testing.T) {
	sshBody := repeat("b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAE", 4)
	cases := []struct {
		name   string
		in     string
		id     string
		secret string
	}{
		{"aws access key id", "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			"aws-access-key-id", "AKIAIOSFODNN7EXAMPLE"},
		{"aws session token key id", "ASIA" + repeat("B", 16),
			"aws-access-key-id", "ASIA" + repeat("B", 16)},
		{"github classic token", "ghp_" + repeat("f", 36),
			"github-token", "ghp_" + repeat("f", 36)},
		{"github fine-grained token", "github_pat_" + repeat("0", 36),
			"github-token", "github_pat_" + repeat("0", 36)},
		{"anthropic key", "sk-ant-api03-" + repeat("C", 40),
			"anthropic-api-key", "sk-ant-api03-" + repeat("C", 40)},
		{"openai key", "sk-proj-" + repeat("d", 40),
			"openai-api-key", "sk-proj-" + repeat("d", 40)},
		{"slack bot token", "xoxb-123456789012-1234567890123456-" + repeat("e", 24),
			"slack-token", "xoxb-123456789012-1234567890123456-"},
		{"google api key", "AIza" + repeat("g", 35),
			"google-api-key", "AIza" + repeat("g", 35)},
		{"rsa private key block", pemBlock("RSA PRIVATE KEY", repeat("h", 64)),
			"private-key-block", repeat("h", 64)},
		{"openssh private key block", pemBlock("OPENSSH PRIVATE KEY", sshBody),
			"private-key-block", sshBody},
		{"plain private key block", pemBlock("PRIVATE KEY", repeat("k", 64)),
			"private-key-block", repeat("k", 64)},
		{"token after a quote", `{"token":"ghp_` + repeat("f", 36) + `"}`,
			"github-token", "ghp_" + repeat("f", 36)},
		{"key after an equals sign", "key=sk-proj-" + repeat("d", 40),
			"openai-api-key", "sk-proj-" + repeat("d", 40)},
		{"key after a colon", `{"key":sk-proj-` + repeat("d", 40) + `}`,
			"openai-api-key", "sk-proj-" + repeat("d", 40)},
		{"key after a space", "Authorization: Bearer sk-ant-api03-" + repeat("C", 40),
			"anthropic-api-key", "sk-ant-api03-" + repeat("C", 40)},
		{"key after a newline", "first line\nsk-proj-" + repeat("d", 40),
			"openai-api-key", "sk-proj-" + repeat("d", 40)},
		{"token after a bracket", "(xoxb-123456789012-123456789012345678-" + repeat("e", 24) + ")",
			"slack-token", "xoxb-123456789012-123456789012345678-"},
		{"env assignment", "DATABASE_PASSWORD=correct-horse-battery",
			"env-assignment", "correct-horse-battery"},
		{"quoted env assignment", `SERVICE_API_KEY="` + repeat("m", 20) + `"`,
			"env-assignment", repeat("m", 20)},
		{"bare token name", "SECRET=nineteen-characters-long",
			"env-assignment", "nineteen-characters-long"},
		{"prefixed secret name", "MY_CLIENT_SECRET=" + repeat("n", 12),
			"env-assignment", repeat("n", 12)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, n := Redact(tc.in)
			if n != 1 {
				t.Fatalf("matches = %d; want 1 for %q", n, tc.in)
			}
			if !strings.Contains(out, Marker(tc.id)) {
				t.Fatalf("output %q lacks marker %q", out, Marker(tc.id))
			}
			if strings.Contains(out, tc.secret) {
				t.Fatalf("output %q still holds the secret text", out)
			}
			again, n2 := Redact(out)
			if again != out || n2 != 0 {
				t.Fatalf("redaction is not stable: %q (%d further matches)", again, n2)
			}
		})
	}
}

func TestRedactLeavesNearMissesAlone(t *testing.T) {
	publicBody := repeat("MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A", 2)
	for _, s := range []string{
		"the quick brown fox jumps over the lazy dog",
		"AKIAIOSFODNN7EXAMPL",
		strings.ToLower("AKIAIOSFODNN7EXAMPLE"),
		"ghp_" + repeat("f", 35),
		"github_pat_" + repeat("0", 29),
		"sk-ant-short",
		"sk-short",
		"the sk- deployment was fine",
		"AIza" + repeat("g", 34),
		"xoxb-tooshort",
		"MAX_TOKENS=4096",
		"TOKEN_EXPIRY=3600",
		"NEXT_PUBLIC_API_URL=https://example.com/app",
		"OLDPWD=/home/agent",
		"PUBLIC_KEY=" + publicBody,
		"API_KEY=",
		"API_KEY=short",
		`API_KEY="short"`,
		"API_KEY=${OTHER_VAR}",
		"-----BEGIN PUBLIC KEY-----\n" + publicBody + "\n-----END PUBLIC KEY-----",
		"-----BEGIN PRIVATE KEY-----\n" + repeat("h", 64),
		"SELECT id FROM events WHERE kind = 'message' AND id > 100",
		"task-add-device-revocation-regression-tests",
		"desk-notification-service-account-rotation",
		"disk-usage-metrics-collector-daemon",
		"ask-clarifying-questions-before-refactor",
		"risk-assessment-checklist-for-release",
		"task-ant-workspace-planner-and-sync",
	} {
		out, n := Redact(s)
		if n != 0 || out != s {
			t.Fatalf("near miss %q became %q with %d matches; want untouched", s, out, n)
		}
	}
}

func TestRedactLeavesTokenShapesWithoutABoundary(t *testing.T) {
	token := "sk-proj-" + repeat("d", 40)
	for _, s := range []string{"x" + token, "-" + token, "_" + token, "7" + token} {
		out, n := Redact(s)
		if n != 0 || out != s {
			t.Fatalf("glued shape %q became %q with %d matches; a key needs a boundary before its prefix", s, out, n)
		}
	}
}

func TestRedactKeepsEnvNameAndBoundary(t *testing.T) {
	out, n := Redact("(GITHUB_TOKEN=abcdefgh1234) && echo done")
	if n != 1 {
		t.Fatalf("matches = %d; want 1", n)
	}
	want := "(GITHUB_TOKEN=" + Marker("env-assignment") + ") && echo done"
	if out != want {
		t.Fatalf("output = %q; want %q", out, want)
	}
}

func TestRedactPrefersPreciseTokenKindInsideAssignment(t *testing.T) {
	token := "sk-ant-api03-" + repeat("C", 40)
	out, n := Redact("ANTHROPIC_API_KEY=" + token)
	if n != 1 {
		t.Fatalf("matches = %d; want the token match only", n)
	}
	want := "ANTHROPIC_API_KEY=" + Marker("anthropic-api-key")
	if out != want {
		t.Fatalf("output = %q; want %q", out, want)
	}
}

func TestRedactMatchesPrivateKeyInsideJsonString(t *testing.T) {
	block := pemBlock("OPENSSH PRIVATE KEY", repeat("z", 64))
	escaped := strings.ReplaceAll(strings.ReplaceAll(block, `\`, `\\`), "\n", `\n`)
	in := `{"type":"user","content":"cat key:\n` + escaped + `\n"}`
	out, n := Redact(in)
	if n != 1 {
		t.Fatalf("matches = %d; want 1 for %q", n, in)
	}
	if !strings.Contains(out, Marker("private-key-block")) {
		t.Fatalf("output lacks the private-key marker: %q", out)
	}
	if strings.Contains(out, repeat("z", 64)) {
		t.Fatalf("output still holds key body text: %q", out)
	}
	if !strings.HasPrefix(out, `{"type":"user","content":"cat key:\n`) {
		t.Fatalf("output lost its JSON prefix: %q", out)
	}
}

func TestRedactCountsAdditively(t *testing.T) {
	in := fmt.Sprintf("keys: AKIAIOSFODNN7EXAMPLE and ghp_%s and AKIAIOSFODNN7EXAMPL", repeat("f", 36))
	out, n := Redact(in)
	if n != 2 {
		t.Fatalf("matches = %d; want 2", n)
	}
	if got := strings.Count(out, "[redacted:"); got != 2 {
		t.Fatalf("markers = %d; want 2 in %q", got, out)
	}
}

func TestCountReportsPerPattern(t *testing.T) {
	counts := Count(fmt.Sprintf("ghp_%s ghp_%s AKIAIOSFODNN7EXAMPLE", repeat("f", 36), repeat("f", 36)))
	if len(counts) != 2 || counts["github-token"] != 2 || counts["aws-access-key-id"] != 1 {
		t.Fatalf("counts = %v; want github-token 2 and aws-access-key-id 1", counts)
	}
}

func TestMarkerAndIDs(t *testing.T) {
	if got := Marker("github-token"); got != "[redacted:github-token]" {
		t.Fatalf("marker = %q", got)
	}
	ids := IDs()
	if len(ids) == 0 || len(ids) != len(unique(ids)) {
		t.Fatalf("pattern ids = %v; want a fixed unique set", ids)
	}
}

func unique(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func TestRedactSkipsStringsWithoutAnchors(t *testing.T) {
	for _, s := range []string{"", "plain text without any anchor shape", strings.Repeat("zz", 500)} {
		if out, n := Redact(s); out != s || n != 0 {
			t.Fatalf("anchor prefilter changed %q", s)
		}
	}
}

func FuzzRedact(f *testing.F) {
	f.Add("AKIAIOSFODNN7EXAMPLE")
	f.Add("export GITHUB_TOKEN=ghp_" + repeat("f", 36))
	f.Add(pemBlock("OPENSSH PRIVATE KEY", repeat("b", 200)))
	f.Add("nested \"json\" with sk-ant-api03-" + repeat("C", 40) + " inside")
	f.Add("-----BEGIN PRIVATE KEY-----")
	f.Add("task-add-device-revocation-regression-tests")
	f.Fuzz(func(t *testing.T, s string) {
		out, n := Redact(s)
		again, n2 := Redact(out)
		if again != out {
			t.Fatalf("unstable redaction:\n first  %q\n second %q", out, again)
		}
		if n2 != 0 {
			t.Fatalf("second pass matched %d times inside %q", n2, out)
		}
		if n > 0 && strings.Count(out, "[redacted:") < n {
			t.Fatalf("count %d but output %q holds fewer markers", n, out)
		}
	})
}
