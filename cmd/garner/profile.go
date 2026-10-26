package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/trucke/garner/internal/profile"
)

// checkProfile checks a profile file, or standard input for "-", without a
// server: it lists the preferences the profile makes, with warnings, or every
// problem. It returns the exit code: 0 when the profile is valid, 1 when
// not.
func checkProfile(path string, stdin io.Reader, out io.Writer) int {
	var text []byte
	var err error
	if path == "-" {
		text, err = io.ReadAll(io.LimitReader(stdin, profile.MaxSize+1))
	} else {
		text, err = readLimited(path)
	}
	if err != nil {
		fmt.Fprintln(out, "✗ "+err.Error())
		return 1
	}
	p, warnings, err := profile.Parse(string(text))
	if err != nil {
		problems := []string{err.Error()}
		if pe := (*profile.Error)(nil); errors.As(err, &pe) {
			problems = pe.Problems
		}
		for _, problem := range problems {
			fmt.Fprintln(out, "✗ "+problem)
		}
		return 1
	}
	preferences := make([]string, 0, len(p.Preferences()))
	for _, c := range p.Preferences() {
		preferences = append(preferences, c.String())
	}
	upgrades := map[bool]string{true: "on", false: "off"}[p.Upgrades]
	fmt.Fprintf(out, "✓ %s\n", p.Name)
	for _, row := range [][2]string{
		{"Preferences", strings.Join(preferences, " → ")},
		{"Dual audio", string(p.DualAudio)},
		{"Unnamed audio", string(p.UnnamedAudio)},
		{"Upgrades", upgrades},
	} {
		fmt.Fprintf(out, "  %-15s %s\n", row[0], row[1])
	}
	for _, w := range warnings {
		fmt.Fprintln(out, "! "+w)
	}
	return 0
}

// readLimited reads a file of at most profile.MaxSize bytes and one more,
// so Parse reports a file that is too large without reading all of it.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, profile.MaxSize+1))
}
