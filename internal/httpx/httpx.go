// Package httpx builds the HTTP clients Garner uses for external services.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
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

type userAgentTransport struct {
	agent string
	next  http.RoundTripper
}

func (t userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", t.agent)
	return t.next.RoundTrip(r)
}
