package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoConfig(remotes string) string {
	return "[core]\n\trepositoryformatversion = 0\n\tbare = false\n" + remotes
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	o, err := Resolve(path)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", path, err)
	}
	if o.Path != path {
		t.Fatalf("Resolve(%s) reports path %s", path, o.Path)
	}
	return o.Repo
}

func TestResolveRepository(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "dioclet")
	writeFile(t, filepath.Join(repo, ".git", "config"), repoConfig(
		"[remote \"upstream\"]\n\turl = https://github.com/other/dioclet.git\n"+
			"[remote \"origin\"]\n\turl = git@github.com:nibzard/dioclet.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"))
	sub := filepath.Join(repo, "web", "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{repo, sub} {
		if got := resolve(t, path); got != "github.com/nibzard/dioclet" {
			t.Errorf("Resolve(%s) = %q, want the origin remote", path, got)
		}
	}
}

func TestResolveWorktreeUsesCommonConfig(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "ohya")
	writeFile(t, filepath.Join(main, ".git", "config"), repoConfig("[remote \"origin\"]\n\turl = https://github.com/nibzard/ohya.git\n"))
	writeFile(t, filepath.Join(main, ".git", "worktrees", "task", "commondir"), "../..\n")
	worktree := filepath.Join(root, "worktrees", "task")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "task")+"\n")
	if got := resolve(t, worktree); got != "github.com/nibzard/ohya" {
		t.Errorf("worktree resolves to %q", got)
	}
}

func TestResolveSubmoduleUsesOwnConfig(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	writeFile(t, filepath.Join(parent, ".git", "config"), repoConfig("[remote \"origin\"]\n\turl = https://github.com/nibzard/parent.git\n"))
	writeFile(t, filepath.Join(parent, ".git", "modules", "lib", "config"), repoConfig("[remote \"origin\"]\n\turl = https://github.com/nibzard/lib.git\n"))
	writeFile(t, filepath.Join(parent, "lib", ".git"), "gitdir: ../.git/modules/lib\n")
	if got := resolve(t, filepath.Join(parent, "lib")); got != "github.com/nibzard/lib" {
		t.Errorf("submodule resolves to %q", got)
	}
}

func TestResolveRemoteChoice(t *testing.T) {
	for _, tc := range []struct {
		name, remotes, want string
	}{
		{"sole remote", "[remote \"github\"]\n\turl = https://github.com/nibzard/a.git\n", "github.com/nibzard/a"},
		{"ambiguous remotes", "[remote \"a\"]\n\turl = https://github.com/x/a\n[remote \"b\"]\n\turl = https://github.com/x/b\n", ""},
		{"no remote", "", ""},
		{"local path remote", "[remote \"origin\"]\n\turl = /srv/git/a.git\n", ""},
		{"relative path remote", "[remote \"origin\"]\n\turl = ../a.git\n", ""},
		{"file remote", "[remote \"origin\"]\n\turl = file:///srv/git/a.git\n", ""},
		{"credentials stripped", "[remote \"origin\"]\n\turl = https://bot:s3cr3t@github.com/x/a.git\n", "github.com/x/a"},
		{"quoted value and comment", "[remote \"origin\"]\n\tURL = \"git@github.com:x/a.git\" ; primary\n", "github.com/x/a"},
		{"first url wins", "[remote \"origin\"]\n\turl = https://github.com/x/first\n\turl = https://github.com/x/second\n", "github.com/x/first"},
		{"legacy section syntax", "[remote.origin]\n\turl = https://github.com/x/a\n", "github.com/x/a"},
		{"section name is case-insensitive", "[Remote \"origin\"]\n\turl = https://github.com/x/a\n", "github.com/x/a"},
		{"subsection is case-sensitive", "[remote \"Origin\"]\n\turl = https://github.com/x/a\n[remote \"b\"]\n\turl = https://github.com/x/b\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, ".git", "config"), repoConfig(tc.remotes))
			if got := resolve(t, repo); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveWithoutRepository(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file.txt")
	writeFile(t, file, "x")
	prunedWorktree := filepath.Join(root, "pruned")
	writeFile(t, filepath.Join(prunedWorktree, ".git"), "gitdir: "+filepath.Join(root, "gone", ".git", "worktrees", "pruned")+"\n")
	for _, path := range []string{
		filepath.Join(root, "deleted-worktree"),
		filepath.Join(file, "below-a-file"),
		plain,
		prunedWorktree,
		"relative/path",
		"",
	} {
		if got := resolve(t, path); got != "" {
			t.Errorf("Resolve(%q) = %q, want no repository", path, got)
		}
	}
}

func TestResolveUnreadableConfigIsTransient(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	repo := t.TempDir()
	config := filepath.Join(repo, ".git", "config")
	writeFile(t, config, repoConfig("[remote \"origin\"]\n\turl = https://github.com/x/a\n"))
	if err := os.Chmod(config, 0); err != nil {
		t.Fatal(err)
	}
	if o, err := Resolve(repo); err == nil {
		t.Fatalf("Resolve of an unreadable config = %+v, want an error", o)
	}
}

func TestResolveOversizedConfigHasNoRepository(t *testing.T) {
	repo := t.TempDir()
	big := make([]byte, maxConfigBytes+1)
	for i := range big {
		big[i] = '#'
	}
	writeFile(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = https://github.com/x/a\n"+string(big))
	if got := resolve(t, repo); got != "" {
		t.Errorf("oversized config resolves to %q", got)
	}
}

func FuzzParseRemotes(f *testing.F) {
	f.Add("[remote \"origin\"]\n\turl = git@github.com:x/a.git\n")
	f.Add("[remote.origin]\nurl=\"a b\" # c\\\nd\n")
	f.Add("[remote \"o\\\"x\"]\n\turl = \\\"q\\\\\n")
	f.Fuzz(func(t *testing.T, config string) {
		a := parseRemotes(config)
		b := parseRemotes(config)
		if len(a) != len(b) {
			t.Fatal("parseRemotes is not deterministic")
		}
		names := map[string]bool{}
		for i, r := range a {
			if r != b[i] {
				t.Fatal("parseRemotes is not deterministic")
			}
			if r.name == "" || r.url == "" || names[r.name] {
				t.Fatalf("parseRemotes returned an empty or repeated remote: %+v", a)
			}
			names[r.name] = true
		}
	})
}
