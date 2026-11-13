package mcpserver

import (
	"fmt"
	"strings"
)

// singleStatement reports whether query holds exactly one SQL statement.
// The tool's contract is one statement per call, and the mattn driver would
// otherwise execute every statement in the input and answer with the last
// one's rows. The scan follows SQLite's lexical rules far enough to find
// statement terminators: quotes, comments and bracketed identifiers never
// terminate a statement.
func singleStatement(query string) error {
	var sawStatement, terminated, sawAfterTerminator bool
	i, n := 0, len(query)
	for i < n {
		switch c := query[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '-' && i+1 < n && query[i+1] == '-': // line comment
			i += 2
			for i < n && query[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && query[i+1] == '*': // block comment
			if end := strings.Index(query[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else { // unterminated: SQLite rejects it at prepare
				i = n
			}
		case c == '\'' || c == '"' || c == '`':
			// Text literal or quoted identifier; the doubling rule ('' "")
			// escapes a repeated quote inside.
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
		case c == '[': // bracketed identifier, closed by the first ]
			i++
			for i < n && query[i] != ']' {
				i++
			}
			if i < n {
				i++
			}
			sawStatement, sawAfterTerminator = mark(sawStatement, terminated, sawAfterTerminator)
		case c == ';':
			// The first terminator ends the one allowed statement; any
			// later one is content after it ("select 1;;" is two).
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

// mark records that a real token appeared, before or after the first
// terminator.
func mark(sawStatement, terminated, sawAfterTerminator bool) (bool, bool) {
	if terminated {
		return sawStatement, true
	}
	return true, sawAfterTerminator
}
