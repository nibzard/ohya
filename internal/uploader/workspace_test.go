package uploader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/workspace"
)

func cwdLine(session, cwd, content string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"message":{"role":"user","content":%q}}`+"\n",
		session, cwd, content)
}

func gitRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = "+remote+"\n")
	return dir
}

func TestUploaderObservesSessionWorkspaces(t *testing.T) {
	f := newFixture(t, Options{})
	hub := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(hub.addr)

	dioclet := gitRepo(t, "https://bot:secret@github.com/nibzard/dioclet.git")
	deleted := filepath.Join(t.TempDir(), "deleted-worktree")
	path := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, path, cwdLine(sessA, dioclet, "one")+cwdLine(sessB, deleted, "two"))
	f.mustSync()

	repo := func(session string) string {
		var r *string
		if err := hub.db.Writer().QueryRow(`SELECT repo FROM sessions WHERE native_id = ?`, session).Scan(&r); err != nil {
			t.Fatal(err)
		}
		if r == nil {
			return ""
		}
		return *r
	}
	if got := repo(sessA); got != "github.com/nibzard/dioclet" {
		t.Fatalf("repo of the session in a git checkout = %q\nlog:\n%s", got, f.log)
	}
	if got := repo(sessB); got != "" {
		t.Fatalf("repo of the session in a deleted directory = %q", got)
	}
	if n := count(t, hub.db, `SELECT count(*) FROM workspaces`); n != 2 {
		t.Fatalf("workspaces = %d, want 2", n)
	}
	if n := count(t, hub.db, `SELECT count(*) FROM workspaces WHERE repo LIKE '%secret%'`); n != 0 {
		t.Fatal("a remote credential reached the hub")
	}

	ohya := gitRepo(t, "git@github.com:nibzard/ohya.git")
	const sessC = "00000000-0000-4000-8000-0000000000c1"
	appendFile(t, path, cwdLine(sessC, filepath.Join(ohya, "internal"), "three"))
	if err := os.MkdirAll(filepath.Join(ohya, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustSync()
	if got := repo(sessC); got != "github.com/nibzard/ohya" {
		t.Fatalf("repo of a later session = %q", got)
	}
}

func TestUploaderRetriesUnreadableWorkspaces(t *testing.T) {
	locked := true
	f := newFixture(t, Options{Resolve: func(p string) (workspace.Observation, error) {
		if locked {
			return workspace.Observation{}, errors.New("permission denied")
		}
		return workspace.Observation{Path: p, Repo: "github.com/x/app"}, nil
	}})
	hub := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(hub.addr)
	path := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, path, cwdLine(sessA, "/work/app", "one"))
	f.mustSync()
	if n := count(t, hub.db, `SELECT count(*) FROM workspaces`); n != 0 {
		t.Fatalf("an unreadable workspace was observed")
	}
	if !strings.Contains(f.log.String(), "could not be read and stay pending") {
		t.Fatalf("log does not report the read failure:\n%s", f.log)
	}
	locked = false
	appendFile(t, path, cwdLine(sessA, "/work/app", "two"))
	f.mustSync()
	if n := count(t, hub.db, `SELECT count(*) FROM sessions WHERE repo = 'github.com/x/app'`); n != 1 {
		t.Fatalf("the pending workspace was not observed on the next upload\nlog:\n%s", f.log)
	}
}

type oldHubConn struct {
	net.Conn
	reply bytes.Buffer
}

func (c *oldHubConn) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == 'W' {
		_ = hubsync.WriteError(&c.reply, hubsync.CodeMalformed, `hubsync: bad frame marker 'W'`)
		c.Conn.Close()
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *oldHubConn) Read(p []byte) (int, error) {
	if c.reply.Len() > 0 {
		return c.reply.Read(p)
	}
	return c.Conn.Read(p)
}

func TestUploaderKeepsUploadingToAnOlderHub(t *testing.T) {
	f := newFixture(t, Options{})
	hub := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(hub.addr)
	f.up.dial = func(ctx context.Context) (net.Conn, error) {
		c, err := f.dialer.dial(ctx)
		if err != nil {
			return nil, err
		}
		return &oldHubConn{Conn: c}, nil
	}
	path := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, path, cwdLine(sessA, "/work/app", "one"))
	f.mustSync()
	if !strings.Contains(f.log.String(), "does not accept workspace observations") {
		t.Fatalf("log does not report the older hub:\n%s", f.log)
	}
	appendFile(t, path, cwdLine(sessA, "/work/app", "two"))
	f.mustSync()
	f.mustSync()
	if n := count(t, hub.db, `SELECT count(*) FROM events`); n != 2 {
		t.Fatalf("events = %d, want both records uploaded", n)
	}
	if strings.Count(f.log.String(), "does not accept workspace observations") != 1 {
		t.Fatalf("the older hub was reported more than once:\n%s", f.log)
	}
}
