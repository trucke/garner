// Package cli is Garner's command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/connect"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/tui"
)

// Execute runs the command line and returns the exit code.
func Execute(version string) int {
	if version == "" {
		version = buildVersion()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot(version).ExecuteContext(ctx); err != nil {
		if errors.Is(err, server.ErrNotRunning) {
			err = fmt.Errorf("Garner isn't running. Start it with %s.", code("garner serve"))
		}
		printError(err)
		return 1
	}
	return 0
}

func buildVersion() string {
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

type app struct {
	version    string
	configPath string
}

func newRoot(version string) *cobra.Command {
	a := &app{version: version}
	root := &cobra.Command{
		Use:   "garner",
		Short: "Self-hosted TV and anime manager",
		Long: "Self-hosted TV and anime manager.\n\nRun garner without a command to open the terminal interface. It keeps\n" +
			"its connection in $GARNER_CONNECTION or ~/.config/garner/connection.",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
				return errors.New("garner opens a terminal interface; run it in a terminal, or see garner --help")
			}
			path, err := connect.DefaultPath()
			if err != nil {
				return err
			}
			return tui.Run(cmd.Context(), path, a.version)
		},
	}
	root.PersistentFlags().StringVarP(&a.configPath, "config", "c", "", "config file (default $GARNER_CONFIG or "+config.DefaultPath+")")
	root.AddCommand(a.serveCmd(), a.tokenCmd(), a.backupCmd(), a.statusCmd())
	return root
}

func (a *app) config() (*config.Config, error) {
	path, explicit := a.configPath, a.configPath != ""
	if !explicit {
		if env := os.Getenv("GARNER_CONFIG"); env != "" {
			path, explicit = env, true
		} else {
			path = config.DefaultPath
		}
	}
	return config.Load(path, explicit)
}

func (a *app) admin() (*server.Admin, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	return server.NewAdmin(cfg.DataDir)
}

func (a *app) serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the Garner server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			logger := charmlog.NewWithOptions(os.Stderr, charmlog.Options{ReportTimestamp: true, TimeFormat: time.DateTime})
			log := slog.New(logger)
			for _, w := range cfg.Warnings() {
				log.Warn(w)
			}
			return server.Run(cmd.Context(), cfg, a.version, log)
		},
	}
}

func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage the tokens clients use to connect"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "create NAME",
			Short: "Create a token for a device, such as laptop",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				admin, err := a.admin()
				if err != nil {
					return err
				}
				t, err := admin.CreateToken(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				success("Created token %s", bold.Render(t.Name))
				lipgloss.Println()
				lipgloss.Println(bold.Render("Connection code"))
				lipgloss.Println(accent.Render(t.Code))
				lipgloss.Println()
				lipgloss.Println(dim.Render("Paste it into Garner on that device. It contains the token."))
				lipgloss.Println()
				lipgloss.Println(bold.Render("Token") + dim.Render(" for scripts and the API"))
				lipgloss.Println(accent.Render(t.Token))
				lipgloss.Println()
				lipgloss.Println(warn.Render("Both are shown only this once."))
				return nil
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List tokens",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				admin, err := a.admin()
				if err != nil {
					return err
				}
				tokens, err := admin.Tokens(cmd.Context())
				if err != nil {
					return err
				}
				if len(tokens) == 0 {
					lipgloss.Println("No tokens yet. Create one with " + code("garner token create NAME") + ".")
					return nil
				}
				t := table.New().
					Border(lipgloss.RoundedBorder()).
					BorderStyle(dim).
					Headers("NAME", "TOKEN", "CREATED", "LAST USED").
					StyleFunc(func(row, _ int) lipgloss.Style {
						if row == table.HeaderRow {
							return bold.Padding(0, 1)
						}
						return lipgloss.NewStyle().Padding(0, 1)
					})
				for _, tok := range tokens {
					used := dim.Render("never")
					if tok.LastUsed != nil {
						used = ago(*tok.LastUsed)
					}
					t.Row(tok.Name, dim.Render("garner_…")+tok.Last4, ago(tok.Created), used)
				}
				lipgloss.Println(t)
				return nil
			},
		},
		&cobra.Command{
			Use:   "revoke NAME",
			Short: "Revoke a token and disconnect its device",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				admin, err := a.admin()
				if err != nil {
					return err
				}
				r, err := admin.RevokeToken(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				closed := ""
				if r.ClosedStreams > 0 {
					closed = " and closed " + plural(r.ClosedStreams, "open connection")
				}
				success("Revoked token %s%s", bold.Render(r.Name), closed)
				return nil
			},
		},
	)
	return cmd
}

func (a *app) backupCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a consistent snapshot of the database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			admin, err := a.admin()
			if err != nil {
				return err
			}
			b, err := admin.Backup(cmd.Context(), output)
			if err != nil {
				return err
			}
			success("Backed up the database %s", dim.Render("("+size(b.Size)+")"))
			fields([2]string{"File", b.Path})
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "backup file (default: backups folder in the data directory)")
	return cmd
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the running server's state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			admin, err := a.admin()
			if err != nil {
				return err
			}
			st, err := admin.Status(cmd.Context())
			if err != nil {
				return err
			}
			lipgloss.Println(good.Render("●") + " " + bold.Render("Garner "+st.Version) + dim.Render(" · running since "+ago(st.Started)))
			lipgloss.Println()
			tls := st.TLS
			switch st.TLS {
			case server.TLSOff:
				tls = dim.Render("off (loopback only)")
			case server.TLSProxy:
				tls = "at the proxy" + dim.Render(" · Garner serves it plain HTTP")
			}
			integrity := good.Render(st.Integrity)
			if st.Integrity != "ok" {
				integrity = bad.Render(st.Integrity)
			}
			backup := warn.Render("never")
			if st.LastBackup != nil {
				backup = ago(st.LastBackup.Created) + dim.Render(" · "+st.LastBackup.Reason+" · "+st.LastBackup.Path)
			}
			rows := [][2]string{
				{"Address", st.URL},
				{"TLS", tls},
			}
			if st.Pin != "" {
				rows = append(rows, [2]string{"Key pin", dim.Render(st.Pin)})
			}
			rows = append(rows,
				[2]string{"Database", size(st.Size) + " · integrity " + integrity + dim.Render(" · "+st.Database)},
				[2]string{"Last backup", backup},
				[2]string{"Tokens", fmt.Sprintf("%d · %s", st.Tokens, plural(st.Streams, "open connection"))},
			)
			fields(rows...)
			return nil
		},
	}
}
