package workspace

import "strings"

type remote struct {
	name string
	url  string
}

func parseRemotes(config string) []remote {
	var remotes []remote
	index := map[string]int{}
	section, subsection := "", ""
	lines := strings.Split(config, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		for hasOddTrailingBackslashes(line) && i+1 < len(lines) {
			i++
			line = line[:len(line)-1] + strings.TrimSuffix(lines[i], "\r")
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			section, subsection = parseSection(line)
			continue
		}
		if section != "remote" || subsection == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		url := parseValue(value)
		if url == "" {
			continue
		}
		if _, seen := index[subsection]; seen {
			continue
		}
		index[subsection] = len(remotes)
		remotes = append(remotes, remote{name: subsection, url: url})
	}
	return remotes
}

func hasOddTrailingBackslashes(s string) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func parseSection(line string) (string, string) {
	end := -1
	inQuote := false
	for i := 1; i < len(line); i++ {
		c := line[i]
		if inQuote {
			if c == '\\' && i+1 < len(line) {
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		if c == '"' {
			inQuote = true
			continue
		}
		if c == ']' {
			end = i
			break
		}
	}
	if end < 0 {
		return "", ""
	}
	header := strings.TrimSpace(line[1:end])
	if sep := strings.IndexAny(header, " \t"); sep >= 0 {
		name := header[:sep]
		rest := strings.TrimSpace(header[sep+1:])
		if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
			return strings.ToLower(name), unescape(rest[1 : len(rest)-1])
		}
		return "", ""
	}
	if name, sub, ok := strings.Cut(header, "."); ok {
		return strings.ToLower(name), strings.ToLower(sub)
	}
	return strings.ToLower(header), ""
}

func parseValue(raw string) string {
	var b strings.Builder
	quoted := false
	pendingSpace := ""
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '"':
			quoted = !quoted
		case !quoted && (c == '#' || c == ';'):
			return strings.TrimSpace(b.String())
		case c == '\\' && i+1 < len(raw):
			i++
			b.WriteString(pendingSpace)
			pendingSpace = ""
			b.WriteByte(escaped(raw[i]))
			continue
		case !quoted && (c == ' ' || c == '\t'):
			if b.Len() > 0 {
				pendingSpace += string(c)
			}
			continue
		default:
			b.WriteString(pendingSpace)
			pendingSpace = ""
			b.WriteByte(c)
			continue
		}
	}
	return b.String()
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escaped(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'b':
		return '\b'
	default:
		return c
	}
}
