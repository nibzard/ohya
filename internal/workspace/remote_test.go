package workspace

import (
	"strings"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/nibzard/ohya.git", "github.com/nibzard/ohya"},
		{"https://github.com/nibzard/ohya", "github.com/nibzard/ohya"},
		{"https://GitHub.Com/Nibzard/Ohya.git", "github.com/Nibzard/Ohya"},
		{"HTTPS://GitHub.Com/Nibzard/Ohya.git", "github.com/Nibzard/Ohya"},
		{"http://example.com/dev/example.git", "example.com/dev/example"},
		{"ssh://git@github.com/nibzard/ohya.git", "github.com/nibzard/ohya"},
		{"git@github.com:nibzard/ohya.git", "github.com/nibzard/ohya"},
		{"git@gitlab.com:group/sub/project.git", "gitlab.com/group/sub/project"},
		{"https://example.com/dev/example/", "example.com/dev/example"},
		{"", ""},
		{"not a url", ""},
		{"example.com", ""},
		{"https://example.com", ""},
		{"git@github.com:nibzard", "github.com/nibzard"},
		{"https://user:token@github.com/nibzard/ohya.git", "github.com/nibzard/ohya"},
		{"https://user:p@ss@github.com/nibzard/ohya.git", "github.com/nibzard/ohya"},
		{"user:secret@github.com:org/repo.git", "github.com/org/repo"},
		{"https://bot:sec/ret@github.com/org/repo.git", ""},
		{"https://github.com/org/repo.git?token=SECRET", "github.com/org/repo"},
		{"https://github.com/org/repo.git#main", "github.com/org/repo"},
		{"git@[2001:db8::1]:org/repo.git", "[2001:db8::1]/org/repo"},
		{"ssh://git@git.example.com:2222/team/app.git", "git.example.com/team/app"},
		{"https://git.example.com:8443/team/app", "git.example.com/team/app"},
		{"ssh://git@[2001:db8::1]:2222/team/app.git", "[2001:db8::1]/team/app"},
		{"git@git.example.com:/srv/team/app.git", "git.example.com/srv/team/app"},
		{"github.com/nibzard/ohya", "github.com/nibzard/ohya"},
		{"/srv/git/app.git", ""},
		{"../myrepo.git", ""},
		{"./myrepo.git", ""},
		{"~/myrepo.git", ""},
		{"file:///srv/git/app.git", ""},
		{"file:/srv/git/app.git", ""},
		{"ftp://example.com/org/repo.git", ""},
		{"s3://bucket/org/repo.git", ""},
		{"https://github.com/nib zard/ohya", ""},
		{"https://github.com/" + strings.Repeat("a", MaxRepoBytes), ""},
	}
	for _, c := range cases {
		if got := NormalizeRemote(c.in); got != c.want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func FuzzNormalizeRemote(f *testing.F) {
	for _, s := range []string{
		"https://github.com/nibzard/ohya.git",
		"git@github.com:nibzard/ohya.git",
		"ssh://git@[2001:db8::1]:2222/team/app.git",
		"https://user:p@ss@github.com/x/y",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := NormalizeRemote(in)
		if out == "" {
			return
		}
		if again := NormalizeRemote(out); again != out {
			t.Fatalf("NormalizeRemote is not idempotent: %q -> %q -> %q", in, out, again)
		}
	})
}
