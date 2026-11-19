package workspace

import (
	"strings"
	"unicode"
)

func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	if u == "" || strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") ||
		strings.HasPrefix(u, "~") || strings.HasPrefix(strings.ToLower(u), "file:") {
		return ""
	}
	var host, path string
	if idx := schemeSeparator(u); idx >= 0 {
		switch strings.ToLower(u[:idx]) {
		case "https", "http", "ssh", "git":
		default:
			return ""
		}
		rest := u[idx+3:]
		var ok bool
		if host, path, ok = strings.Cut(rest, "/"); !ok {
			return ""
		}
		host = withoutPort(withoutUserinfo(host))
	} else {
		head := u
		if slash := strings.IndexByte(u, '/'); slash >= 0 {
			head = u[:slash]
		}
		if at := strings.LastIndexByte(head, '@'); at >= 0 {
			u = u[at+1:]
		}
		skip := 0
		if strings.HasPrefix(u, "[") {
			end := strings.IndexByte(u, ']')
			if end < 0 {
				return ""
			}
			skip = end + 1
		}
		colon := indexFrom(u, ':', skip)
		slash := indexFrom(u, '/', skip)
		switch {
		case colon >= 0 && (slash < 0 || colon < slash):
			host, path = u[:colon], u[colon+1:]
		case slash >= 0:
			host, path = u[:slash], u[slash+1:]
		default:
			return ""
		}
	}
	if cut := strings.IndexAny(path, "?#"); cut >= 0 {
		path = path[:cut]
	}
	for trimmed := ""; trimmed != path; {
		trimmed = path
		path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	}
	path = strings.TrimLeft(path, "/")
	if !validHost(host) || path == "" || strings.IndexFunc(path, unsafePathRune) >= 0 {
		return ""
	}
	out := strings.ToLower(host) + "/" + path
	if len(out) > MaxRepoBytes {
		return ""
	}
	return out
}

func schemeSeparator(u string) int {
	idx := strings.Index(u, "://")
	if idx <= 0 {
		return -1
	}
	if strings.IndexByte(u[:idx], '/') >= 0 || strings.IndexByte(u[:idx], '@') >= 0 {
		return -1
	}
	return idx
}

func withoutUserinfo(authority string) string {
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		return authority[at+1:]
	}
	return authority
}

func withoutPort(host string) string {
	tail := host
	if bracket := strings.LastIndexByte(host, ']'); bracket >= 0 {
		tail = host[bracket:]
	}
	if colon := strings.LastIndexByte(tail, ':'); colon >= 0 {
		port := tail[colon+1:]
		if isDigits(port) {
			return host[:len(host)-len(tail)+colon]
		}
	}
	return host
}

func isDigits(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func indexFrom(s string, c byte, from int) int {
	if i := strings.IndexByte(s[from:], c); i >= 0 {
		return from + i
	}
	return -1
}

func validHost(host string) bool {
	if inner, ok := strings.CutPrefix(host, "["); ok {
		inner, ok = strings.CutSuffix(inner, "]")
		return ok && inner != "" && strings.Trim(inner, "0123456789abcdefABCDEF:.") == ""
	}
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	return strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_") == ""
}

func unsafePathRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || r == unicode.ReplacementChar
}
