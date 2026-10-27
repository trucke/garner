package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/trucke/garner/internal/profile"
)

func (a *app) profileCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "Manage the profiles that say which releases shows accept"}
	var replace bool
	imp := &cobra.Command{
		Use:   "import",
		Short: "Import a profile from standard input",
		Long: "Import a profile from standard input, such as\n" +
			"  garnerd profile import < german.toml\n" +
			"A profile of the same name stays unless --replace is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			text, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), profile.MaxSize+1))
			if err != nil {
				return err
			}
			admin, err := a.admin()
			if err != nil {
				return err
			}
			res, err := admin.ImportProfile(cmd.Context(), string(text), replace)
			if pe := (*profile.Error)(nil); errors.As(err, &pe) {
				for _, p := range pe.Problems {
					printError(errors.New(p))
				}
				return fmt.Errorf("the profile has %s; nothing was imported", count(len(pe.Problems), "problem"))
			}
			if err != nil {
				return err
			}
			switch {
			case !res.Replaced:
				success("Imported %s", bold.Render(res.Name))
			case len(res.Shows) == 0:
				success("Replaced %s", bold.Render(res.Name))
			case len(res.Shows) == 1:
				success("Replaced %s · %s follows the new settings", bold.Render(res.Name), res.Shows[0])
			default:
				success("Replaced %s · %s follow the new settings: %s", bold.Render(res.Name),
					count(len(res.Shows), "show"), strings.Join(res.Shows, ", "))
			}
			for _, w := range res.Warnings {
				lipgloss.Println(warn.Render("!") + " " + w)
			}
			return nil
		},
	}
	imp.Flags().BoolVar(&replace, "replace", false, "replace a profile of the same name")
	cmd.AddCommand(imp)
	return cmd
}

// count writes n with a noun, such as "1 show" or "3 shows".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
