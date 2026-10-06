package sabnzbd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Responses follow SABnzbd 5.1's shapes, with made-up names.
func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Errorf("request %s %s; want a POST without query", r.Method, r.URL)
		}
		if r.PostFormValue("apikey") != "key" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "API Key Incorrect")
			return
		}
		switch r.PostFormValue("mode") {
		case "addurl":
			if r.PostFormValue("name") == "http://bad" {
				fmt.Fprint(w, `{"status":false,"error":"Unable to fetch"}`)
				return
			}
			fmt.Fprint(w, `{"status":true,"nzo_ids":["job-1"]}`)
		case "queue":
			fmt.Fprint(w, `{"queue":{"status":"Downloading","slots":[{"status":"Downloading","nzo_id":"job-1",
				"filename":"Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP","cat":"tv","percentage":"42","timeleft":"0:01:10","time_added":1791300000}]}}`)
		case "history":
			fmt.Fprint(w, `{"history":{"slots":[{"nzo_id":"job-2","name":"Harbor.Precinct.S01E02.German.DL.1080p.WEB-GRP","category":"tv",
				"status":"Failed","fail_message":"Aborted, cannot be completed - https://sabnzbd.org/not-complete","storage":"","bytes":0,
				"time_added":1791300000,"action_line":"","stage_log":[{"name":"Download","actions":["Downloaded in 1 second"]}]}]}}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL, "key", srv.Client())

	id, err := c.AddURL(ctx, "http://prowlarr/1/download?apikey=x", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP", "tv")
	if err != nil || id != "job-1" {
		t.Fatalf("AddURL = %q, %v", id, err)
	}
	var refused *Error
	if _, err := c.AddURL(ctx, "http://bad", "x", "tv"); !errors.As(err, &refused) || refused.Message != "Unable to fetch" {
		t.Errorf("refused AddURL error = %v", err)
	}
	queue, err := c.Queue(ctx, "")
	if err != nil || len(queue) != 1 || queue[0].Percent != 42 || queue[0].Status != "Downloading" || queue[0].Added.IsZero() {
		t.Errorf("Queue = %+v, %v", queue, err)
	}
	history, err := c.History(ctx, []string{"job-2"}, "")
	if err != nil || len(history) != 1 || history[0].Status != "Failed" || len(history[0].Stages) != 1 {
		t.Errorf("History = %+v, %v", history, err)
	}
	if _, err := New(srv.URL, "wrong", srv.Client()).Queue(ctx, ""); !errors.As(err, &refused) {
		t.Errorf("wrong key error = %v", err)
	}
}
