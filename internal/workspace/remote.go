package workspace

import (
	"strings"
	"unicode"
)

func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	for trimmed := ""; trimmed != u; {
		trimmed = u
		u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	}
	scheme := false
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		if rest, ok := strings.CutPrefix(u, prefix); ok {
			u, scheme = rest, true
			break
		}
	}
	var host, path string
	if scheme {
		var ok bool
		if host, path, ok = strings.Cut(u, "/"); !ok {
			return ""
		}
		host = withoutPort(withoutUserinfo(host))
	} else {
		skip := 0
		if strings.HasPrefix(u, "[") {
			skip = strings.IndexByte(u, ']') + 1
		}
		colon := indexFrom(u, ':', skip)
		slash := indexFrom(u, '/', skip)
		switch {
		case colon >= 0 && (slash < 0 || colon < slash):
			host, path = withoutUserinfo(u[:colon]), u[colon+1:]
		case slash >= 0:
			host, path = withoutUserinfo(u[:slash]), u[slash+1:]
		default:
			return ""
		}
	}
	path = strings.TrimLeft(path, "/")
	if !validHost(host) || path == "" || strings.IndexFunc(path, unsafePathRune) >= 0 {
		return ""
	}
	return strings.ToLower(host) + "/" + path
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
		return host[:len(host)-len(tail)+colon]
	}
	return host
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
	return host != "" && strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_") == ""
}

func unsafePathRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || r == unicode.ReplacementChar
}
