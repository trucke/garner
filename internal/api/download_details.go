package api

import (
	"html"
	"regexp"
	"strings"
)

var (
	logBreak        = regexp.MustCompile(`(?i)<br\s*/?\s*>|</(?:p|div)>`)
	logTag          = regexp.MustCompile(`(?i)</?(?:a|b|code|div|em|font|i|p|span|strong|u)(?:\s[^>]*)?\s*>`)
	missingArticles = regexp.MustCompile(`(?i)^([1-9][0-9]*) (articles?) (?:were|was) missing[.!]?$`)
)

// Lines reads SABnzbd's HTML log as plain lines. It also reads older logs
// from before the server saved them as plain text. Callers must redact
// credentials after decoding HTML, and clean terminal control characters
// before displaying the lines.
func (s Stage) Lines() []string {
	lines := []string{}
	for _, action := range s.Actions {
		plain := html.UnescapeString(logTag.ReplaceAllString(logBreak.ReplaceAllString(action, "\n"), ""))
		for _, line := range strings.FieldsFunc(plain, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
}

// SummarizeFailure prefers concrete download evidence to SABnzbd's generic
// incomplete-job message. Other failures keep their original explanation;
// missing articles must not hide a disk, permission or file-check failure.
func (d Download) SummarizeFailure() string {
	failure := strings.ToLower(d.Failure)
	if d.State != Failed || d.Import.State != "" || !strings.HasPrefix(failure, "aborted, cannot be completed") {
		return ""
	}
	for _, stage := range d.Stages {
		if stage.Name != "Download" {
			continue
		}
		for _, line := range stage.Lines() {
			if match := missingArticles.FindStringSubmatch(line); match != nil {
				return match[1] + " " + strings.ToLower(match[2]) + " missing"
			}
		}
	}
	return ""
}
