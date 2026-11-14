package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func check(t *testing.T, root string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(root, false, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCleanTreePasses(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/a.go": "package a\n\nconst raw = `// not a comment\n/* also not */`\nconst quoted = \"// nor this /* one */\"\n",
	})
	code, out, errOut := check(t, root)
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q; want clean pass", code, out, errOut)
	}
}

func TestExplanatoryCommentsFail(t *testing.T) {
	cases := map[string]struct {
		src  string
		line int
	}{
		"doc line comment": {"package a\n\n// F does things.\nfunc F() {}\n", 3},
		"inline comment":   {"package a\n\nfunc F() int {\n\treturn 1 // one\n}\n", 4},
		"block comment":    {"package a\n\n/*\nblock\n*/\nfunc F() {}\n", 3},
		"inline block":     {"package a\n\nfunc F(/* none */) {}\n", 3},
		"test file":        {"package a\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\t// check\n}\n", 6},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"a/a_test.go": tc.src})
			code, out, _ := check(t, root)
			want := "a/a_test.go:" + strconv.Itoa(tc.line) + ": explanatory comment:"
			if code != 1 || !strings.HasPrefix(out, want) {
				t.Fatalf("code = %d, stdout = %q; want exit 1 and prefix %q", code, out, want)
			}
		})
	}
}

func TestApprovedDirectivePasses(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/a.go": "package a\n\nimport \"embed\"\n\n//go:embed *.sql\nvar files embed.FS\n",
	})
	if code, out, _ := check(t, root); code != 0 {
		t.Fatalf("code = %d, stdout = %q; want approved directive to pass", code, out)
	}
}

func TestUnapprovedDirectivesFail(t *testing.T) {
	for _, directive := range []string{"//go:generate stringer", "//go:linkname x y", "//go:embed", "//nolint:all", "//line a.go:1"} {
		t.Run(directive, func(t *testing.T) {
			root := writeTree(t, map[string]string{"a/a.go": "package a\n\n" + directive + "\nvar x int\n"})
			code, out, _ := check(t, root)
			if code != 1 || !strings.Contains(out, "unapproved-directive comment") {
				t.Fatalf("code = %d, stdout = %q; want unapproved directive failure", code, out)
			}
		})
	}
}

func TestPlatformSpecificFilesAreChecked(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/a.go":         "package a\n",
		"a/a_windows.go": "package a\n\n// windows only\nvar w int\n",
		"a/a_plan9.go":   "//go:build plan9\n\npackage a\n",
	})
	code, out, _ := check(t, root)
	if code != 1 || !strings.Contains(out, "a/a_windows.go:3: explanatory") || !strings.Contains(out, "a/a_plan9.go:1: unapproved-directive") {
		t.Fatalf("code = %d, stdout = %q; want both platform files reported", code, out)
	}
}

func TestSkippedDirectoriesAreIgnored(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/a.go":            "package a\n",
		"a/testdata/x.go":   "package x\n// fixture\n",
		".git/x.go":         "package x\n// hidden\n",
		"vendor/v/v.go":     "package v\n// vendored\n",
		"_scratch/s/s.go":   "package s\n// ignored by go tooling\n",
		"a/testdata/bad.go": "not go",
	})
	if code, out, errOut := check(t, root); code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q; want skipped directories ignored", code, out, errOut)
	}
}

func TestUnparsableFileFails(t *testing.T) {
	root := writeTree(t, map[string]string{"a/a.go": "package a\nfunc {"})
	code, _, errOut := check(t, root)
	if code != 2 || !strings.Contains(errOut, "a.go") {
		t.Fatalf("code = %d, stderr = %q; want parse failure with exit 2", code, errOut)
	}
}

func TestUnreadableRootFails(t *testing.T) {
	code, _, errOut := check(t, filepath.Join(t.TempDir(), "missing"))
	if code != 2 || errOut == "" {
		t.Fatalf("code = %d, stderr = %q; want exit 2", code, errOut)
	}
}

func TestEmptyTreeFails(t *testing.T) {
	code, _, errOut := check(t, t.TempDir())
	if code != 2 || !strings.Contains(errOut, "no Go files") {
		t.Fatalf("code = %d, stderr = %q; want exit 2 for an empty tree", code, errOut)
	}
}

func TestInventoryListsEveryComment(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/a.go":      "package a\n\nimport \"embed\"\n\n//go:embed *.sql\nvar files embed.FS\n\nvar x = 1 // inline\n",
		"a/a_test.go": "package a\n\n/* block */\n",
	})
	var stdout, stderr bytes.Buffer
	if code := run(root, true, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	want := "a/a.go:5\ta\tsource\tapproved-directive\t//go:embed *.sql\n" +
		"a/a.go:8\ta\tsource\texplanatory\t// inline\n" +
		"a/a_test.go:3\ta\ttest\texplanatory\t/* block */\n"
	if stdout.String() != want {
		t.Fatalf("inventory =\n%s\nwant\n%s", stdout.String(), want)
	}
}
