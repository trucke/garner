// Package sabnzbd hands releases to SABnzbd and reads its queue and history.
package sabnzbd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trucke/garner/internal/httpx"
)

// Error is a refusal SABnzbd answered with, as opposed to no answer at
// all. After an Error, SABnzbd did not act on the request.
type Error struct{ Message string }

func (e *Error) Error() string { return "SABnzbd: " + e.Message }

// Client talks to one SABnzbd instance. Every call is a form POST with the
// API key in the body, so URLs and the errors quoting them hold no key.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a client for the SABnzbd at baseURL.
func New(baseURL, apiKey string, client *http.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: apiKey, http: client}
}

// AddURL tells SABnzbd to fetch the NZB at link itself and returns the new
// job's ID. link holds Prowlarr's API key; it travels in the request body.
func (c *Client) AddURL(ctx context.Context, link, name, category string) (string, error) {
	var out struct {
		NzoIDs []string `json:"nzo_ids"`
	}
	if err := c.call(ctx, url.Values{"mode": {"addurl"}, "name": {link}, "nzbname": {name}, "cat": {category}}, &out); err != nil {
		return "", err
	}
	if len(out.NzoIDs) == 0 || out.NzoIDs[0] == "" {
		return "", errors.New("SABnzbd did not return a job ID")
	}
	return out.NzoIDs[0], nil
}

// Job is a download in SABnzbd's queue.
type Job struct {
	ID       string
	Name     string
	Category string
	// Status is SABnzbd's own, such as Downloading, Queued or Paused.
	Status   string
	Percent  float64
	TimeLeft string
	Added    time.Time
}

// Queue lists queued jobs, all of them or those whose names contain search.
func (c *Client) Queue(ctx context.Context, search string) ([]Job, error) {
	var out struct {
		Queue struct {
			Slots []struct {
				NzoID      string `json:"nzo_id"`
				Filename   string `json:"filename"`
				Cat        string `json:"cat"`
				Status     string `json:"status"`
				Percentage string `json:"percentage"`
				TimeLeft   string `json:"timeleft"`
				TimeAdded  int64  `json:"time_added"`
			} `json:"slots"`
		} `json:"queue"`
	}
	params := url.Values{"mode": {"queue"}, "limit": {"0"}}
	if search != "" {
		params.Set("search", search)
	}
	if err := c.call(ctx, params, &out); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(out.Queue.Slots))
	for _, s := range out.Queue.Slots {
		pct, _ := strconv.ParseFloat(s.Percentage, 64)
		jobs = append(jobs, Job{
			ID: s.NzoID, Name: s.Filename, Category: s.Cat, Status: s.Status, Percent: pct,
			TimeLeft: s.TimeLeft, Added: time.Unix(s.TimeAdded, 0),
		})
	}
	return jobs, nil
}

// Done is a job in SABnzbd's history: finished, failed or still in
// post-processing.
type Done struct {
	ID       string
	Name     string
	Category string
	// Status is Completed, Failed, or a post-processing step such as
	// Repairing or Extracting.
	Status string
	// Action describes the running post-processing step.
	Action string
	// FailMessage is SABnzbd's reason for a failure.
	FailMessage string
	// Storage is where SABnzbd put the files, as SABnzbd sees its disk.
	Storage string
	Bytes   int64
	Added   time.Time
	Stages  []Stage
}

// Stage is a post-processing step and what happened in it.
type Stage struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// History lists history jobs with the given IDs, or whose names contain
// search when ids is empty.
func (c *Client) History(ctx context.Context, ids []string, search string) ([]Done, error) {
	var out struct {
		History struct {
			Slots []struct {
				NzoID       string  `json:"nzo_id"`
				Name        string  `json:"name"`
				Category    string  `json:"category"`
				Status      string  `json:"status"`
				ActionLine  string  `json:"action_line"`
				FailMessage string  `json:"fail_message"`
				Storage     string  `json:"storage"`
				Bytes       int64   `json:"bytes"`
				TimeAdded   int64   `json:"time_added"`
				StageLog    []Stage `json:"stage_log"`
			} `json:"slots"`
		} `json:"history"`
	}
	params := url.Values{"mode": {"history"}, "limit": {"0"}}
	switch {
	case len(ids) > 0:
		params.Set("nzo_ids", strings.Join(ids, ","))
	case search != "":
		params.Set("search", search)
	}
	if err := c.call(ctx, params, &out); err != nil {
		return nil, err
	}
	jobs := make([]Done, 0, len(out.History.Slots))
	for _, s := range out.History.Slots {
		jobs = append(jobs, Done{
			ID: s.NzoID, Name: s.Name, Category: s.Category, Status: s.Status, Action: s.ActionLine,
			FailMessage: s.FailMessage, Storage: s.Storage, Bytes: s.Bytes, Added: time.Unix(s.TimeAdded, 0), Stages: s.StageLog,
		})
	}
	return jobs, nil
}

func (c *Client) call(ctx context.Context, params url.Values, out any) error {
	params.Set("output", "json")
	params.Set("apikey", c.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api", strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return httpx.Redact(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return httpx.Redact(err)
	}
	if resp.StatusCode == http.StatusForbidden {
		return &Error{"refused the API key (" + strings.TrimSpace(string(body)) + ")"}
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{"answered " + resp.Status}
	}
	var status struct {
		Status *bool  `json:"status"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(body, &status) == nil && (status.Error != "" || status.Status != nil && !*status.Status) {
		return &Error{cmp.Or(status.Error, "request failed")}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unreadable SABnzbd response: %w", err)
	}
	return nil
}
