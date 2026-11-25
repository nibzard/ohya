package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nibzard/ohya/internal/buildinfo"
	"github.com/nibzard/ohya/internal/observe"
)

func buildIdentity(revisionOverride string, dirtyOverride bool) (string, bool) {
	_, revision, dirty, _ := buildinfo.Identity()
	if revisionOverride != "" {
		revision = revisionOverride
	}
	return revision, dirty == "true" || dirtyOverride
}

func buildName(revision string, dirty bool) string {
	name := revision
	if name == "" {
		name = "unknown"
	}
	if dirty {
		name += " with uncommitted changes"
	}
	return name
}

type staleMCP struct {
	pid      int
	revision string
	dirty    bool
}

func checkMCPBuilds(observeDir, revision string, dirty bool, nowMS int64) []finding {
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
		if s.Instance.BuildRevision != revision || s.Instance.BuildDirty || dirty {
			stale = append(stale, staleMCP{pid: s.Instance.PID, revision: s.Instance.BuildRevision, dirty: s.Instance.BuildDirty})
		}
	}
	if live == 0 {
		return nil
	}
	this := buildName(revision, dirty)
	if len(stale) == 0 {
		return []finding{{Check: "mcp_build", Severity: sevOK,
			Detail: fmt.Sprintf("%s this build (%s)", plural(live, "running ohya mcp process runs", "running ohya mcp processes run"), this)}}
	}
	slices.SortFunc(stale, func(a, b staleMCP) int { return a.pid - b.pid })
	parts := make([]string, len(stale))
	alike := false
	for i, s := range stale {
		parts[i] = fmt.Sprintf("PID %d build %s", s.pid, buildName(s.revision, s.dirty))
		if s.revision == revision && s.dirty && dirty {
			alike = true
		}
	}
	verb := "run"
	if len(stale) == 1 {
		verb = "runs"
	}
	detail := fmt.Sprintf("%d of %d running ohya mcp processes %s another build than this one (%s): %s; their agent sessions still get the query engine, redaction, and error hints of that build",
		len(stale), live, verb, this, strings.Join(parts, ", "))
	if alike {
		detail += "; two uncommitted builds of one revision cannot be told apart, so that process can also be this build"
	}
	return []finding{{Check: "mcp_build", Severity: sevWarn,
		Detail: detail,
		Remedy: "reconnect the ohya MCP server in each agent session that uses it (in Claude Code run /mcp) or start a new session; setup never stops coding sessions"}}
}
