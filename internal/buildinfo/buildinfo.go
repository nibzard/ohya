package buildinfo

import (
	"runtime"
	"runtime/debug"
)

var ReleaseVersion string

func Identity() (version, revision, dirty, goVersion string) {
	version, revision, dirty, goVersion = "devel", "unknown", "unknown", runtime.Version()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		if ReleaseVersion != "" {
			version = ReleaseVersion
		}
		return
	}
	goVersion = info.GoVersion
	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
	if ReleaseVersion != "" {
		version = ReleaseVersion
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				revision = shortRevision(s.Value)
			}
		case "vcs.modified":
			dirty = s.Value
		}
	}
	return
}

func shortRevision(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
