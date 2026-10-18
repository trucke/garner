// Package buildinfo names the version of the running program.
package buildinfo

import "runtime/debug"

// Version names a build: the release, such as "v0.1.0 (1cfab60)", when the
// build set version and commit with -ldflags "-X main.version=… -X
// main.commit=…"; otherwise the module version, or "dev-" and the commit
// of a build from a checkout.
func Version(version, commit string) string {
	if version != "" {
		if len(commit) >= 7 {
			return version + " (" + commit[:7] + ")"
		}
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return "dev-" + s.Value[:12]
		}
	}
	return "dev"
}
