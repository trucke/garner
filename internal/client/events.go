package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Event is something the server reported, or a change of the stream itself.
type Event struct {
	Type string
	Data json.RawMessage
}

// Events Watch adds to the server's own.
const (
	// EventConnected follows every (re)connect. The server does not replay
	// missed events, so reload the current state.
	EventConnected = "garner.connected"
	// EventDisconnected means the stream dropped; Watch tries again.
	EventDisconnected = "garner.disconnected"
	// EventUnauthorized means the token stopped working; Watch stops.
	EventUnauthorized = "garner.unauthorized"
)

const (
	reconnectAfter = 3 * time.Second
	// silentFor drops a stream that sent nothing, not even the server's
	// 25-second ping, for this long.
	silentFor = 60 * time.Second
	maxEvent  = 4 << 20
)

// Watch delivers the server's events until ctx ends or the token stops
// working, then closes the channel. It reconnects on its own and reports
// each change of the connection as an event.
func (c *Client) Watch(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		send := func(e Event) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		online := true // report the first failure too
		for {
			connected, err := c.stream(ctx, send)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrUnauthorized) {
				send(Event{Type: EventUnauthorized})
				return
			}
			if (connected || online) && !send(Event{Type: EventDisconnected}) {
				return
			}
			online = false
			select {
			case <-time.After(reconnectAfter):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// stream reads one connection's events. connected reports whether the
// server accepted it.
func (c *Client) stream(ctx context.Context, send func(Event) bool) (connected bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(silentFor, cancel)
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.JoinPath("/api/v1/events").String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, c.classify(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return false, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("the event stream answered %s", resp.Status)
	}
	if !send(Event{Type: EventConnected}) {
		return true, ctx.Err()
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxEvent)
	var typ string
	var data strings.Builder
	for sc.Scan() {
		watchdog.Reset(silentFor)
		line := sc.Text()
		switch {
		case line == "":
			if typ != "" && !send(Event{Type: typ, Data: json.RawMessage(data.String())}) {
				return true, ctx.Err()
			}
			typ = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, errors.New("the server closed the event stream")
}
