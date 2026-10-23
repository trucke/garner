package api

import (
	"slices"
	"testing"
)

func TestFailureSummary(t *testing.T) {
	for _, tc := range []struct {
		name, failure, evidence, imported, want string
	}{
		{"missing articles", "Aborted, cannot be completed", "Downloaded in 12 seconds<br/>27 articles were missing", "", "27 articles missing"},
		{"one missing article", "Aborted, cannot be completed", "1 article was missing", "", "1 article missing"},
		{"no missing articles", "Aborted, cannot be completed", "0 articles were missing", "", ""},
		{"no evidence", "Aborted, cannot be completed", "Downloaded in 12 seconds", "", ""},
		{"disk failure", "Unpacking failed: disk is full", "27 articles were missing", "", ""},
		{"unknown failure", "An unfamiliar server failure", "27 articles were missing", "", ""},
		{"file check", "file check: no German track", "27 articles were missing", ImportRejected, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Download{State: Failed, Failure: tc.failure, Import: Import{State: tc.imported},
				Stages: []Stage{{Name: "Download", Actions: []string{tc.evidence}}}}
			if got := d.SummarizeFailure(); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
			if d.Failure != tc.failure {
				t.Error("summarizing replaced the original error")
			}
		})
	}
}

func TestStageLines(t *testing.T) {
	stage := Stage{Actions: []string{
		"Downloaded in 12 seconds<BR />Age: 210d<br>27 articles were missing",
		"<b>Repair</b>: not enough blocks &amp; no recovery possible\r\nNext line",
		"A name with &lt;literal brackets&gt;",
		"\n  \n",
	}}
	want := []string{"Downloaded in 12 seconds", "Age: 210d", "27 articles were missing",
		"Repair: not enough blocks & no recovery possible", "Next line", "A name with <literal brackets>"}
	got := stage.Lines()
	if !slices.Equal(got, want) {
		t.Errorf("log lines = %q, want %q", got, want)
	}
	for _, actions := range [][]string{nil, {}, {"\n <br/> \n"}} {
		if (Stage{Actions: actions}).Lines() == nil {
			t.Error("an empty log must encode its actions as an array, not null")
		}
	}
	// The store also reads already-normalized logs from recent downloads.
	if again := (Stage{Actions: got}).Lines(); !slices.Equal(again, want) {
		t.Errorf("reading a normalized log changed its evidence: %q", again)
	}
}
