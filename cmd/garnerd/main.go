// Command garnerd is Garner's server, with the commands that manage it on
// its machine: tokens, backups and status.
package main

import (
	"os"

	"github.com/trucke/garner/internal/buildinfo"
)

// version is set at build time with -ldflags "-X main.version=…".
var version string

func main() {
	os.Exit(execute(buildinfo.Version(version)))
}
