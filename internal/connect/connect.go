// Package connect encodes what a client needs to reach the server: its
// address, a token and, for Garner's own certificate, the key's pin. The code
// travels over the same trusted channel as the token, so clients never have
// to accept an unknown fingerprint.
package connect

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/trucke/garner/internal/token"
)

const prefix = "garner1:"

// Code is a connection code. It contains the token, so it is a secret.
type Code struct {
	URL   string `json:"u"`
	Token string `json:"t"`
	// Pin is set when the server uses its own certificate; empty means
	// verify the certificate against the system's authorities.
	Pin string `json:"p,omitempty"`
}

// String encodes the code for display.
func (c Code) String() string {
	b, _ := json.Marshal(c)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Parse decodes a connection code.
func Parse(s string) (Code, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !ok {
		return Code{}, errors.New("not a Garner connection code")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Code{}, errors.New("connection code is damaged")
	}
	var c Code
	if err := json.Unmarshal(b, &c); err != nil {
		return Code{}, errors.New("connection code is damaged")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Code{}, fmt.Errorf("connection code has an invalid server address %q", c.URL)
	}
	if !token.WellFormed(c.Token) {
		return Code{}, errors.New("connection code has no valid token")
	}
	if c.Pin != "" && u.Scheme != "https" {
		return Code{}, errors.New("connection code pins a certificate for a plain HTTP address")
	}
	return c, nil
}

// DefaultPath is where a client keeps its connection: $GARNER_CONNECTION, or
// garner/connection in $XDG_CONFIG_HOME or ~/.config on every platform.
func DefaultPath() (string, error) {
	if p := os.Getenv("GARNER_CONNECTION"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "garner", "connection"), nil
}

// Save writes the code to path, readable only by the user. The code holds
// the token, so the file is as secret as the token itself.
func Save(path string, c Code) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// A leftover file would keep its old mode; start fresh so 0600 applies.
	os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(c.String()+"\n"), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Load reads a code written by Save. The error wraps os.ErrNotExist when
// there is none.
func Load(path string) (Code, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Code{}, err
	}
	c, err := Parse(string(b))
	if err != nil {
		return Code{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
