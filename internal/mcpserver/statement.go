package mcpserver

import (
	"fmt"
	"strings"
)

func singleStatement(query string) error {
	var sawStatement, terminated, sawAfterTerminator bool
	i, n := 0, len(query)
	for i < n {
		switch c := query[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '-' && i+1 < n && query[i+1] == '-':
			i += 2
			for i < n && query[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && query[i+1] == '*':
			if end := strings.Index(query[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = n
			}
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			for i < n {
				if query[i] == quote {
					if i+1 < n && query[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			sawStatement, sawAfterTerminator = mark(sawStatement, terminated, sawAfterTerminator)
		case c == '[':
			i++
			for i < n && query[i] != ']' {
				i++
			}
			if i < n {
				i++
			}
			sawStatement, sawAfterTerminator = mark(sawStatement, terminated, sawAfterTerminator)
		case c == ';':
			if terminated {
				sawAfterTerminator = true
			} else {
				terminated = true
			}
			i++
		default:
			sawStatement, sawAfterTerminator = mark(sawStatement, terminated, sawAfterTerminator)
			i++
		}
	}
	switch {
	case sawAfterTerminator:
		return fmt.Errorf("%w: input holds more than one", ErrMultipleStatements)
	case !sawStatement:
		return fmt.Errorf("%w: the input holds no statement", ErrEmptyQuery)
	default:
		return nil
	}
}

func mark(sawStatement, terminated, sawAfterTerminator bool) (bool, bool) {
	if terminated {
		return sawStatement, true
	}
	return true, sawAfterTerminator
}
