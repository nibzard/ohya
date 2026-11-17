package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrNoExecStart = errors.New("unit holds no ExecStart")

type UnitExecStart struct {
	Unit string
	Path string
	Exec string
	Args []string
}

func unescapeSystemdWord(word string) string {
	word = strings.ReplaceAll(word, "%%", "%")
	return strings.ReplaceAll(word, "$$", "$")
}

func systemdWords(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(line); {
		switch ch := line[i]; {
		case ch == ' ' || ch == '\t':
			flush()
			i++
		case ch == '"' || ch == '\'':
			quote := ch
			i++
			inWord = true
			closed := false
			for i < len(line) {
				if quote == '"' && line[i] == '\\' && i+1 < len(line) {
					cur.WriteByte(line[i+1])
					i += 2
					continue
				}
				if line[i] == quote {
					closed = true
					i++
					break
				}
				cur.WriteByte(line[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quote in ExecStart %q", line)
			}
		case ch == '\\':
			if i+1 >= len(line) {
				return nil, fmt.Errorf("trailing backslash in ExecStart %q", line)
			}
			cur.WriteByte(line[i+1])
			i += 2
			inWord = true
		default:
			cur.WriteByte(ch)
			i++
			inWord = true
		}
	}
	flush()
	return words, nil
}

func ParseUnitExecStart(content []byte) (string, []string, error) {
	section := ""
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || section != "Service" || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		value = strings.TrimLeft(strings.TrimSpace(value), "-+!")
		words, err := systemdWords(value)
		if err != nil {
			return "", nil, err
		}
		if len(words) == 0 {
			return "", nil, errors.New("ExecStart holds no command")
		}
		for i := range words {
			words[i] = unescapeSystemdWord(words[i])
		}
		return words[0], words[1:], nil
	}
	return "", nil, ErrNoExecStart
}

func sameExecutable(a, b string) bool {
	ra, aErr := filepath.EvalSymlinks(a)
	rb, bErr := filepath.EvalSymlinks(b)
	if aErr == nil && bErr == nil {
		return ra == rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func ScanServeUnits(dir, bin string) ([]UnitExecStart, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.service"))
	if err != nil {
		return nil, err
	}
	var units []UnitExecStart
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		exec, args, err := ParseUnitExecStart(content)
		if err != nil || len(args) == 0 || args[0] != "serve" || !sameExecutable(exec, bin) {
			continue
		}
		units = append(units, UnitExecStart{Unit: filepath.Base(path), Path: path, Exec: exec, Args: args})
	}
	return units, nil
}
