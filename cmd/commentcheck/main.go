package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var approvedDirectives = []string{"//go:embed "}

type comment struct {
	Path      string
	Line      int
	Package   string
	Test      bool
	Directive bool
	Approved  bool
	Text      string
}

func main() {
	inventory := flag.Bool("inventory", false, "list every comment instead of failing on explanatory comments")
	flag.Parse()
	root := "."
	if flag.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: commentcheck [-inventory] [root]")
		os.Exit(2)
	}
	if flag.NArg() == 1 {
		root = flag.Arg(0)
	}
	os.Exit(run(root, *inventory, os.Stdout, os.Stderr))
}

func run(root string, inventory bool, stdout, stderr io.Writer) int {
	comments, err := scan(root)
	if err != nil {
		fmt.Fprintln(stderr, "commentcheck:", err)
		return 2
	}
	if inventory {
		for _, c := range comments {
			fmt.Fprintf(stdout, "%s:%d\t%s\t%s\t%s\t%s\n", c.Path, c.Line, c.Package, fileKind(c), commentKind(c), firstLine(c.Text))
		}
		return 0
	}
	failed := false
	for _, c := range comments {
		if !c.Approved {
			failed = true
			fmt.Fprintf(stdout, "%s:%d: %s comment: %s\n", c.Path, c.Line, commentKind(c), firstLine(c.Text))
		}
	}
	if failed {
		return 1
	}
	return 0
}

func scan(root string) ([]comment, error) {
	paths, err := goFiles(root)
	if err != nil {
		return nil, err
	}
	var comments []comment
	for _, path := range paths {
		found, err := scanFile(root, path)
		if err != nil {
			return nil, err
		}
		comments = append(comments, found...)
	}
	return comments, nil
}

func goFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skippedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("no Go files found under " + root)
	}
	sort.Strings(paths)
	return paths, nil
}

func skippedDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor"
}

func scanFile(root, path string) ([]comment, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	rel = filepath.ToSlash(rel)
	var comments []comment
	for _, group := range file.Comments {
		for _, c := range group.List {
			comments = append(comments, newComment(rel, fset, file, c))
		}
	}
	return comments, nil
}

func newComment(rel string, fset *token.FileSet, file *ast.File, c *ast.Comment) comment {
	return comment{
		Path:      rel,
		Line:      fset.Position(c.Slash).Line,
		Package:   file.Name.Name,
		Test:      strings.HasSuffix(rel, "_test.go"),
		Directive: isDirective(c.Text),
		Approved:  isApproved(c.Text),
		Text:      c.Text,
	}
}

func isDirective(text string) bool {
	body, ok := strings.CutPrefix(text, "//")
	if !ok {
		body, _ = strings.CutPrefix(text, "/*")
	}
	if strings.HasPrefix(body, "line ") || strings.HasPrefix(body, " +build") {
		return true
	}
	name, _, found := strings.Cut(body, ":")
	if !found || name == "" {
		return false
	}
	for _, r := range name {
		if !('a' <= r && r <= 'z' || '0' <= r && r <= '9') {
			return false
		}
	}
	return true
}

func isApproved(text string) bool {
	for _, prefix := range approvedDirectives {
		if strings.HasPrefix(text, prefix) && len(text) > len(prefix) {
			return true
		}
	}
	return false
}

func fileKind(c comment) string {
	if c.Test {
		return "test"
	}
	return "source"
}

func commentKind(c comment) string {
	switch {
	case c.Approved:
		return "approved-directive"
	case c.Directive:
		return "unapproved-directive"
	default:
		return "explanatory"
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
