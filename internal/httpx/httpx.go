// Package httpx builds the HTTP clients Garner uses for external services.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// New returns a client with a timeout, the given User-Agent and, when caFile
// is set, an extra trusted certificate authority. Redirects to another
// origin are refused so credentials in headers or URLs cannot leak there.
func New(userAgent, caFile string, timeout time.Duration) (*http.Client, error) {
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
		}
		tlsConf.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConf
	return &http.Client{
		Timeout:   timeout,
		Transport: userAgentTransport{userAgent, transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			first := via[0].URL
			if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
				return errors.New("refusing redirect to another origin")
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}, nil
}

// sensitiveParams carry credentials, or links that embed them, such as the
// apikey and link parameters of Prowlarr download links.
var sensitiveParams = map[string]bool{"apikey": true, "api_key": true, "key": true, "token": true, "link": true, "nzbkey": true, "r": true}

// RedactURL hides credentials in a URL: sensitive query values and any
// password. Unparsable input is hidden entirely.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparsable URL]"
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	}
	q := u.Query()
	changed := false
	for k := range q {
		if sensitiveParams[strings.ToLower(k)] {
			q.Set(k, "REDACTED")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// Redact hides credentials in the URL that net/http quotes in request
// errors, so the error can be logged.
func Redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return &url.Error{Op: ue.Op, URL: RedactURL(ue.URL), Err: ue.Err}
	}
	return err
}

type userAgentTransport struct {
	agent string
	next  http.RoundTripper
}

func (t userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", t.agent)
	return t.next.RoundTrip(r)
}
