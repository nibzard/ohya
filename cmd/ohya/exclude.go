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
	for _, form := range excludePathForms(path) {
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
