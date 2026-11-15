package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxConfigBytes = 1 << 20

const maxPointerBytes = 4 << 10

const MaxRepoBytes = 1024

type Observation struct {
	Path string
	Repo string
}

func Resolve(path string) (Observation, error) {
	none := Observation{Path: path}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return none, nil
	}
	if _, err := os.Stat(path); err != nil {
		if gone(err) {
			return none, nil
		}
		return none, fmt.Errorf("workspace: %s: %w", path, err)
	}
	gitDir, err := findGitDir(filepath.Clean(path))
	if err != nil || gitDir == "" {
		return none, err
	}
	config, err := readBounded(filepath.Join(commonDir(gitDir), "config"), maxConfigBytes)
	switch {
	case gone(err) || errors.Is(err, errTooLarge):
		return none, nil
	case err != nil:
		return none, fmt.Errorf("workspace: %s: read git config: %w", path, err)
	}
	if repo := repoFromRemote(remoteURL(parseRemotes(string(config)))); len(repo) <= MaxRepoBytes {
		none.Repo = repo
	}
	return none, nil
}

func findGitDir(dir string) (string, error) {
	for {
		dotGit := filepath.Join(dir, ".git")
		info, err := os.Lstat(dotGit)
		switch {
		case err == nil && info.IsDir():
			return dotGit, nil
		case err == nil && info.Mode().IsRegular():
			return pointedGitDir(dir, dotGit)
		case err != nil && !gone(err):
			return "", fmt.Errorf("workspace: inspect %s: %w", dotGit, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

func pointedGitDir(dir, dotGit string) (string, error) {
	data, err := readBounded(dotGit, maxPointerBytes)
	switch {
	case gone(err) || errors.Is(err, errTooLarge):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("workspace: read %s: %w", dotGit, err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target), nil
}

func commonDir(gitDir string) string {
	data, err := readBounded(filepath.Join(gitDir, "commondir"), maxPointerBytes)
	common := strings.TrimSpace(string(data))
	if err != nil || common == "" {
		return gitDir
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return filepath.Clean(common)
}

func remoteURL(remotes []remote) string {
	for _, r := range remotes {
		if r.name == "origin" {
			return r.url
		}
	}
	if len(remotes) == 1 {
		return remotes[0].url
	}
	return ""
}

func repoFromRemote(url string) string {
	if strings.HasPrefix(url, "/") || strings.HasPrefix(url, ".") ||
		strings.HasPrefix(url, "~") || strings.HasPrefix(strings.ToLower(url), "file:") {
		return ""
	}
	return NormalizeRemote(url)
}

var errTooLarge = errors.New("workspace: file exceeds its size bound")

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errTooLarge
	}
	return data, nil
}

func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR)
}
