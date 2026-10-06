package main

import (
	"os"

	"github.com/trucke/garner/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=…".
var version string

func main() {
	os.Exit(cli.Execute(version))
}
