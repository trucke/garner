// Command garner is Garner's terminal interface. It connects to a Garner
// server (garnerd) on this or another machine and holds none of the
// server's code.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"

	"github.com/trucke/garner/internal/buildinfo"
	"github.com/trucke/garner/internal/connect"
	"github.com/trucke/garner/internal/tui"
)

// version is set at build time with -ldflags "-X main.version=…".
var version string

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), "Garner's terminal interface. It keeps its connection to the server in\n"+
			"$GARNER_CONNECTION or ~/.config/garner/connection.\n\nUsage: garner [--version]\n")
	}
	flag.Parse()
	v := buildinfo.Version(version)
	switch {
	case *showVersion:
		fmt.Println("garner", v)
		return
	case flag.NArg() > 0:
		flag.Usage()
		os.Exit(2)
	}
	if err := run(v); err != nil {
		fmt.Fprintln(os.Stderr, "✗ "+err.Error())
		os.Exit(1)
	}
}

func run(version string) error {
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("garner opens a terminal interface; run it in a terminal, or see garner --help")
	}
	path, err := connect.DefaultPath()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return tui.Run(ctx, path, version)
}
