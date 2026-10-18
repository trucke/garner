// Package buildinfo names the version of the running program.
package buildinfo

import "runtime/debug"

// Version is set, when the build set it with -ldflags "-X main.version=…";
// otherwise the module version, or "dev-" and the commit of a build from a
// checkout.
func Version(set string) string {
	if set != "" {
		return set
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
