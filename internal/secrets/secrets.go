package secrets

import (
	"regexp"
	"strings"
)

type Pattern struct {
	ID   string
	re   *regexp.Regexp
	keep []int
}

var table = []Pattern{
	{ID: "aws-access-key-id",
		re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`)},
	{ID: "github-token",
		re: regexp.MustCompile(`ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{30,}`)},
	{ID: "anthropic-api-key",
		re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`)},
	{ID: "openai-api-key",
		re: regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{ID: "slack-token",
		re: regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{20,}`)},
	{ID: "google-api-key",
		re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{ID: "private-key-block",
		re: regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----.*?-----END (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`)},
	{ID: "env-assignment",
		re:   regexp.MustCompile(`(^|[^A-Za-z0-9_])((?:[A-Z][A-Z0-9_]*)?(?:API_KEY|ACCESS_KEY|SECRET_KEY|PRIVATE_KEY|TOKEN|SECRET|PASSWORD|PASSPHRASE|PASSWD)=)("[^"\n]{8,}"|'[^'\n]{8,}'|[^"'\s\[{),;{$][^"'\s\[{),;]{7,})`),
		keep: []int{1, 2}},
}

var anchors = []string{
	"AKIA", "ASIA", "ghp_", "github_pat_", "sk-", "xox", "AIza",
	"PRIVATE KEY", "=",
}

func Marker(id string) string {
	return "[redacted:" + id + "]"
}

func IDs() []string {
	ids := make([]string, len(table))
	for i, p := range table {
		ids[i] = p.ID
	}
	return ids
}

func anchored(s string) bool {
	for _, a := range anchors {
		if strings.Contains(s, a) {
			return true
		}
	}
	return false
}

func apply(s string, onMatch func(id string)) string {
	if !anchored(s) {
		return s
	}
	for _, p := range table {
		locs := p.re.FindAllStringSubmatchIndex(s, -1)
		if len(locs) == 0 {
			continue
		}
		var b strings.Builder
		last := 0
		for _, loc := range locs {
			b.WriteString(s[last:loc[0]])
			for _, g := range p.keep {
				if loc[2*g] < 0 {
					continue
				}
				b.WriteString(s[loc[2*g]:loc[2*g+1]])
			}
			b.WriteString(Marker(p.ID))
			onMatch(p.ID)
			last = loc[1]
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	return s
}

func Redact(s string) (string, int) {
	total := 0
	out := apply(s, func(string) { total++ })
	return out, total
}

func Count(s string) map[string]int {
	counts := map[string]int{}
	apply(s, func(id string) { counts[id]++ })
	return counts
}
