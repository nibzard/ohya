package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nibzard/ohya/internal/observe"
)

type staleMCP struct {
	pid      int
	revision string
}

func checkMCPBuilds(observeDir, revision string, nowMS int64) []finding {
	if revision == "" || revision == "unknown" {
		return nil
	}
	views, _, err := observe.DiscoverInstances(observeDir)
	if err != nil {
		return nil
	}
	live := 0
	var stale []staleMCP
	for _, view := range views {
		s := view.Snapshot
		if s == nil || s.Stopped || !slices.Contains(s.Instance.Roles, string(observe.RoleMCP)) {
			continue
		}
		age := nowMS - s.SampledWallMS
		if age < 0 || age > observe.SnapshotStaleAge.Milliseconds() || observe.ProbePID(s.Instance.PID) != "running" {
			continue
		}
		live++
		if s.Instance.BuildRevision != revision {
			stale = append(stale, staleMCP{pid: s.Instance.PID, revision: s.Instance.BuildRevision})
		}
	}
	if live == 0 {
		return nil
	}
	if len(stale) == 0 {
		return []finding{{Check: "mcp_build", Severity: sevOK,
			Detail: fmt.Sprintf("%s this build (%s)", plural(live, "running ohya mcp process runs", "running ohya mcp processes run"), revision)}}
	}
	slices.SortFunc(stale, func(a, b staleMCP) int { return a.pid - b.pid })
	parts := make([]string, len(stale))
	for i, s := range stale {
		parts[i] = fmt.Sprintf("PID %d build %s", s.pid, s.revision)
	}
	verb := "run"
	if len(stale) == 1 {
		verb = "runs"
	}
	return []finding{{Check: "mcp_build", Severity: sevWarn,
		Detail: fmt.Sprintf("%d of %d running ohya mcp processes %s another build than this one (%s): %s; their agent sessions still get the query engine, redaction, and error hints of that build",
			len(stale), live, verb, revision, strings.Join(parts, ", ")),
		Remedy: "reconnect the ohya MCP server in each agent session that uses it (in Claude Code run /mcp) or start a new session; setup never stops coding sessions"}}
}
