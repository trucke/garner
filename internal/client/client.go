// Package client talks to a Garner server for the terminal interface. It
// sends the token only to a server it has verified: over HTTPS with the
// pinned or a CA-verified certificate, or over plain HTTP to this machine.
// It never follows redirects and ignores proxy settings, so the token cannot
// be passed on to another address.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/trucke/garner/internal/connect"
	"github.com/trucke/garner/internal/tlsident"
)

// ErrUnauthorized means the server does not know the token, usually because
// it was revoked.
var ErrUnauthorized = errors.New("the server does not know this token")

// ErrNotLoopback means plain HTTP would have left this machine, so nothing
// was sent.
var ErrNotLoopback = errors.New("plain HTTP is allowed only to this machine")

// UnreachableError means no connection to the server could be made.
type UnreachableError struct {
	Addr string
	Err  error
}

func (e *UnreachableError) Error() string { return e.Addr + " did not answer" }
func (e *UnreachableError) Unwrap() error { return e.Err }

// UntrustedError means the server could not be verified, so the token was
// not sent. ServerPin is set when it presented a key other than the pinned
// one.
type UntrustedError struct {
	Addr      string
	ServerPin string
	Err       error
}

func (e *UntrustedError) Error() string {
	switch {
	case e.ServerPin != "":
		return "the server's certificate does not match the pin"
	case errors.Is(e.Err, ErrNotLoopback):
		return ErrNotLoopback.Error()
	}
	return "the server's certificate is not trusted: " + e.Err.Error()
}

func (e *UntrustedError) Unwrap() error { return e.Err }

// ServerError is an error the server answered with.
type ServerError struct {
	Status  int
	Message string
}

func (e *ServerError) Error() string { return e.Message }

// Client is a connection to one server.
type Client struct {
	base   *url.URL
	addr   string
	token  string
	dialer *net.Dialer
	tls    *tls.Config // nil for plain HTTP
	http   *http.Client
}

// New prepares a client for the server in the connection code. It does not
// contact the server.
func New(c connect.Code) (*Client, error) {
	base, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	cl := &Client{
		base:   base,
		addr:   base.Host,
		token:  c.Token,
		dialer: &net.Dialer{Timeout: 10 * time.Second},
	}
	switch {
	case base.Scheme == "http":
		cl.dialer.Control = loopbackOnly
	case c.Pin != "":
		cl.tls = tlsident.ClientConfig(c.Pin, base.Hostname())
		verify := cl.tls.VerifyConnection
		cl.tls.VerifyConnection = func(cs tls.ConnectionState) error {
			if err := verify(cs); err != nil {
				return &rejectedError{err}
			}
			return nil
		}
	default:
		cl.tls = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: base.Hostname()}
	}
	cl.http = &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         cl.dialer.DialContext,
			TLSClientConfig:     cl.tls,
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return cl, nil
}

// Addr is the server's host and port, for display.
func (c *Client) Addr() string { return c.addr }

// loopbackOnly runs after name resolution, on the address actually dialled.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.Unmap().IsLoopback() {
		return ErrNotLoopback
	}
	return nil
}

// Check reaches the server and verifies its certificate without sending the
// token.
func (c *Client) Check(ctx context.Context) error {
	conn, err := c.dialer.DialContext(ctx, "tcp", c.dialAddr())
	if err != nil {
		return c.classify(err)
	}
	defer conn.Close()
	if c.tls == nil {
		return nil
	}
	cfg := c.tls.Clone()
	if cfg.ServerName == "" && net.ParseIP(c.base.Hostname()) == nil {
		cfg.ServerName = c.base.Hostname()
	}
	if err := tls.Client(conn, cfg).HandshakeContext(ctx); err != nil {
		return c.classify(err)
	}
	return nil
}

func (c *Client) dialAddr() string {
	if c.base.Port() != "" {
		return c.base.Host
	}
	if c.base.Scheme == "https" {
		return net.JoinHostPort(c.base.Hostname(), "443")
	}
	return net.JoinHostPort(c.base.Hostname(), "80")
}

// rejectedError marks a pinned server that failed verification, so it is
// reported as untrusted rather than unreachable.
type rejectedError struct{ err error }

func (e *rejectedError) Error() string { return e.err.Error() }
func (e *rejectedError) Unwrap() error { return e.err }

// classify turns transport errors into the errors this package documents.
func (c *Client) classify(err error) error {
	var mismatch *tlsident.PinMismatchError
	var rejected *rejectedError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &mismatch):
		return &UntrustedError{Addr: c.addr, ServerPin: mismatch.Server, Err: err}
	case errors.Is(err, ErrNotLoopback), errors.As(err, &rejected), errors.As(err, &verify):
		return &UntrustedError{Addr: c.addr, Err: err}
	}
	return &UnreachableError{Addr: c.addr, Err: err}
}

// Status is what the server reports about itself and the token in use.
type Status struct {
	Version string `json:"version"`
	Token   string `json:"token"`
}

// Status asks the server for its version and the token's name. It is the
// first request that sends the token.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodGet, "/api/v1/status", nil, &s)
}

const maxResponse = 16 << 20

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	// JoinPath would escape the query's "?" into the path.
	p, query, _ := strings.Cut(path, "?")
	u := c.base.JoinPath(p)
	u.RawQuery = query
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.classify(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return &UnreachableError{Addr: c.addr, Err: err}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return &ServerError{Status: resp.StatusCode, Message: "the server answered with a redirect, which Garner does not follow"}
	case resp.StatusCode >= 400:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = fmt.Sprintf("the server answered %s", resp.Status)
		}
		return &ServerError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the server's answer could not be read: %w", err)
	}
	return nil
}
