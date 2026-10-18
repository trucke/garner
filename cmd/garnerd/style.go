package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Basic ANSI colours follow the user's terminal theme.
var (
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	good    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	bad     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	accent  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	command = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

func success(format string, args ...any) {
	lipgloss.Println(good.Render("✓") + " " + fmt.Sprintf(format, args...))
}

func printError(err error) {
	lipgloss.Fprintln(os.Stderr, bad.Render("✗")+" "+err.Error())
}

// fields prints aligned label/value lines.
func fields(rows ...[2]string) {
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	label := dim.Width(width + 2)
	for _, r := range rows {
		lipgloss.Println("  " + label.Render(r[0]) + r[1])
	}
}

func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
	return t.Format(time.DateOnly)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func code(s string) string { return command.Render("`" + strings.TrimSpace(s) + "`") }
