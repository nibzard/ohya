package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nibzard/ohya/internal/durablefile"
)

const profileBegin = "# >>> ohya PATH >>>"
const profileEnd = "# <<< ohya PATH <<<"

type Profile struct {
	Path      string
	Content   []byte
	WholeFile bool
}

type profileReceipt struct {
	Path      string `json:"path"`
	Content   []byte `json:"content"`
	Previous  []byte `json:"previous,omitempty"`
	Created   bool   `json:"created"`
	WholeFile bool   `json:"whole_file"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func ProfilePlan(home, shell, bin string) ([]Profile, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(bin) || strings.ContainsAny(bin, ":\x00\r\n") {
		return nil, errors.New("PATH setup needs absolute paths without colons or control characters")
	}
	name := filepath.Base(shell)
	if name == "." || name == "" {
		name = "sh"
	}
	if name == "fish" {
		escaped := strings.ReplaceAll(strings.ReplaceAll(bin, "\\", "\\\\"), "'", "\\'")
		return []Profile{{Path: filepath.Join(home, ".config", "fish", "conf.d", "ohya.fish"), Content: []byte("if not contains -- '" + escaped + "' $PATH\n    set -gx PATH '" + escaped + "' $PATH\nend\n"), WholeFile: true}}, nil
	}
	block := []byte("\n" + profileBegin + "\ncase \":$PATH:\" in\n    *" + shellQuote(":"+bin+":") + "*) ;;\n    *) export PATH=" + shellQuote(bin) + ":\"$PATH\" ;;\nesac\n" + profileEnd + "\n")
	var paths []string
	switch name {
	case "zsh":
		paths = []string{filepath.Join(home, ".zshrc")}
	case "bash":
		login := filepath.Join(home, ".profile")
		for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
			path := filepath.Join(home, name)
			if _, err := os.Lstat(path); err == nil {
				login = path
				break
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
		paths = []string{filepath.Join(home, ".bashrc"), login}
	case "sh", "dash", "ksh":
		paths = []string{filepath.Join(home, ".profile")}
	default:
		return nil, fmt.Errorf("shell %s needs manual PATH setup; use --no-modify-path", name)
	}
	var profiles []Profile
	for _, path := range paths {
		profiles = append(profiles, Profile{Path: path, Content: block})
	}
	return profiles, nil
}

func profileReceiptPath(state, path string) string {
	return filepath.Join(state, "setup", "profiles", filepath.Base(path)+".json")
}

func loadProfileReceipt(path string) (profileReceipt, error) {
	raw, err := readRegular(path)
	if err != nil {
		return profileReceipt{}, err
	}
	var r profileReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if !filepath.IsAbs(r.Path) || len(r.Content) == 0 {
		return r, errors.New("invalid profile receipt")
	}
	return r, nil
}

func profileBase(raw []byte, r profileReceipt) ([]byte, error) {
	for _, block := range [][]byte{r.Content, r.Previous} {
		if len(block) > 0 && bytes.Count(raw, block) == 1 && bytes.Count(raw, []byte(profileBegin)) == 1 && bytes.Count(raw, []byte(profileEnd)) == 1 {
			return bytes.Replace(raw, block, nil, 1), nil
		}
	}
	if bytes.Contains(raw, []byte(profileBegin)) || bytes.Contains(raw, []byte(profileEnd)) {
		return nil, errors.New("ohya PATH block has been edited; preserve it")
	}
	return raw, nil
}

func ApplyProfile(state string, p Profile) (bool, error) {
	receiptPath := profileReceiptPath(state, p.Path)
	if p.WholeFile {
		return ApplyFile(p.Path, receiptPath, p.Content)
	}
	raw, err := readRegular(p.Path)
	created := os.IsNotExist(err)
	if err != nil && !created {
		return false, err
	}
	r, err := loadProfileReceipt(receiptPath)
	base := raw
	switch {
	case err == nil:
		if r.Path != p.Path || r.WholeFile {
			return false, errors.New("profile receipt belongs to another file")
		}
		created = r.Created
		base, err = profileBase(raw, r)
		if err != nil {
			return false, err
		}
	case os.IsNotExist(err):
		if bytes.Contains(raw, []byte(profileBegin)) || bytes.Contains(raw, []byte(profileEnd)) {
			return false, errors.New("profile already contains an unowned ohya PATH block")
		}
	default:
		return false, err
	}
	updated := append(bytes.Clone(base), p.Content...)
	if bytes.Equal(raw, updated) {
		return false, nil
	}
	previous := r.Content
	if len(r.Previous) > 0 && bytes.Contains(raw, r.Previous) {
		previous = r.Previous
	}
	r = profileReceipt{Path: p.Path, Content: p.Content, Previous: previous, Created: created}
	encoded, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0700); err != nil {
		return false, err
	}
	if err := durablefile.Write(receiptPath, encoded); err != nil {
		return false, err
	}
	latest, err := readRegular(p.Path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if !bytes.Equal(raw, latest) {
		return false, errors.New("profile changed during setup")
	}
	return true, durablefile.Write(p.Path, updated)
}

func RemoveProfiles(state string) ([]Result, error) {
	receipts, err := filepath.Glob(filepath.Join(state, "setup", "profiles", "*.json"))
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, path := range receipts {
		result := Result{Component: "path", Status: "removed"}
		raw, err := readRegular(path)
		if err != nil {
			return results, err
		}
		var header struct {
			WholeFile bool `json:"whole_file"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return results, err
		}
		if header.WholeFile {
			result.Changed, err = RemoveFile(path)
		} else {
			var r profileReceipt
			r, err = loadProfileReceipt(path)
			if err == nil {
				var current, base []byte
				current, err = readRegular(r.Path)
				if os.IsNotExist(err) {
					err = nil
				}
				if err == nil {
					base, err = profileBase(current, r)
				}
				if err == nil && r.Created && len(base) == 0 {
					err = os.Remove(r.Path)
					if os.IsNotExist(err) {
						err = nil
					}
				} else if err == nil && !bytes.Equal(current, base) {
					err = durablefile.Write(r.Path, base)
				}
				if err == nil {
					err = os.Remove(path)
					result.Changed = true
				}
			}
		}
		if err != nil {
			result.Status = "failed"
			result.Detail = err.Error()
		}
		results = append(results, result)
	}
	return results, nil
}
