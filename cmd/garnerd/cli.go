package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	charmlog "charm.land/log/v2"
	"github.com/spf13/cobra"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/server"
)

// execute runs the command line and returns the exit code.
func execute(version string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot(version).ExecuteContext(ctx); err != nil {
		if errors.Is(err, server.ErrNotRunning) {
			err = fmt.Errorf("Garner isn't running. Start it with %s.", code("garnerd serve"))
		}
		printError(err)
		return 1
	}
	return 0
}

type app struct {
	version    string
	configPath string
}

func newRoot(version string) *cobra.Command {
	a := &app{version: version}
	root := &cobra.Command{
		Use:   "garnerd",
		Short: "Garner's server",
		Long: "Garner's server. garnerd serve runs it; the other commands manage the server\n" +
			"running on this machine. The terminal interface is the separate program garner.",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
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
					lipgloss.Println("No tokens yet. Create one with " + code("garnerd token create NAME") + ".")
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
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the running server's state",
		Long: "Show the running server's state. With --json, for scripts: work.idle says whether a\n" +
			"restart would interrupt nothing, as an upgrade needs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			admin, err := a.admin()
			if err != nil {
				return err
			}
			st, err := admin.Status(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				out := json.NewEncoder(os.Stdout)
				out.SetIndent("", "  ")
				return out.Encode(st)
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
				[2]string{"Work", work(st.Work)},
			)
			fields(rows...)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the state as JSON")
	return cmd
}

// work says what runs, such as "2 downloads · 1 import", or "idle".
func work(w server.Work) string {
	if w.Downloads+w.Imports+w.Searches == 0 {
		return "idle"
	}
	var parts []string
	for _, p := range []struct {
		n    int
		noun string
	}{{w.Downloads, "download"}, {w.Imports, "import"}, {w.Searches, "show search"}} {
		if p.n > 0 {
			parts = append(parts, plural(p.n, p.noun))
		}
	}
	return strings.Join(parts, " · ")
}
