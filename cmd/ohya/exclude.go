package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
)

type excludeGlobs []string

func (g *excludeGlobs) String() string {
	return strings.Join(*g, "\n")
}

func (g *excludeGlobs) Set(value string) error {
	if value == "" {
		*g = nil
		return nil
	}
	globs := strings.Split(value, "\n")
	for _, glob := range globs {
		if err := validExcludeGlob(glob); err != nil {
			return err
		}
	}
	*g = append(*g, globs...)
	return nil
}

func validExcludeGlob(glob string) error {
	switch {
	case glob == "":
		return errors.New("--exclude glob is empty")
	case strings.ContainsAny(glob, "\r\x00"):
		return fmt.Errorf("--exclude glob %q contains a control character", glob)
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return fmt.Errorf("--exclude glob %q: %w", glob, err)
	}
	return nil
}

func newExcludeFlag(fs *flag.FlagSet) *excludeGlobs {
	globs := &excludeGlobs{}
	fs.Var(globs, "exclude", "repeatable `glob` skipped by discovery; path.Match semantics, matched against absolute source paths; empty clears the list")
	return globs
}

func (g excludeGlobs) Match(path string) (bool, error) {
	if len(g) == 0 {
		return false, nil
	}
	return g.matchForms(excludePathForms(path))
}

func (g excludeGlobs) matchRegular(path string, cache dirEvalCache) (bool, error) {
	if len(g) == 0 {
		return false, nil
	}
	return g.matchForms(cache.regularPathForms(path))
}

func (g excludeGlobs) matchForms(forms []string) (bool, error) {
	for _, form := range forms {
		for _, glob := range g {
			matched, err := filepath.Match(glob, form)
			if err != nil {
				return false, fmt.Errorf("--exclude glob %q: %w", glob, err)
			}
			if matched {
				return true, nil
			}
		}
	}
	return false, nil
}

type resolvedDirEntry struct {
	path string
	ok   bool
}

type dirEvalCache map[string]resolvedDirEntry

func (c dirEvalCache) regularPathForms(path string) []string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	forms := []string{abs}
	dir, base := filepath.Split(abs)
	cleanDir := filepath.Clean(dir)
	entry, cached := c[cleanDir]
	if !cached {
		resolved, err := filepath.EvalSymlinks(cleanDir)
		entry = resolvedDirEntry{path: resolved, ok: err == nil}
		c[cleanDir] = entry
	}
	if entry.ok {
		resolved := filepath.Join(entry.path, base)
		if resolved != abs {
			forms = append(forms, resolved)
		}
	}
	return forms
}

func excludePathForms(path string) []string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	forms := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		forms = append(forms, resolved)
	}
	return forms
}
