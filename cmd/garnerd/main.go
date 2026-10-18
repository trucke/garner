// Command garnerd is Garner's server, with the commands that manage it on
// its machine: tokens, backups and status.
package main

import (
	"os"

	"github.com/trucke/garner/internal/buildinfo"
)

// version and commit are set for a release with -ldflags "-X
// main.version=… -X main.commit=…".
var version, commit string

func main() {
	os.Exit(execute(buildinfo.Version(version, commit)))
}
