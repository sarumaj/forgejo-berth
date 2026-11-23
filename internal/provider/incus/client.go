// SPDX-License-Identifier: GPL-3.0-or-later

// Package incus is the incus provider: every host is an Incus server reached through its
// REST API (HTTPS with a client certificate, or the local unix socket), every slot one of its
// containers, restored from a snapshot before every job. The slots' reset cycles run in the
// broker (slot.Local) with the cycle's Incus logic on an API instance: the hosts need Incus
// alone. The API is spoken with net/http.
package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// maxBody is the most read of an answer: an instance's state is a few KiB, an exec's
// output and the container's log are cut there.
const maxBody = 8 << 20

// Client is a client of an Incus server's API, in one project.
type Client struct {
	http    *http.Client
	base    string
	project string
}

// NewClient makes a client of the server at address: unix:///path (the local socket, where
// the broker's user is trusted by being let in), or https://host:port with the broker's
// client certificate, the server checked against tls.ca_file (its own certificate, pinned,
// or the CA that signed it).
func NewClient(address string, t config.TLS, project string) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	c := &Client{http: &http.Client{Transport: transport}, project: project}
	switch u.Scheme {
	case "unix":
		path := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
		c.base = "http://incus"
	case "https":
		tc, err := provider.TLSConfig(t)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tc
		c.base = "https://" + u.Host
	default:
		return nil, fmt.Errorf("%s: unix:///path or https://host:port", address)
	}
	return c, nil
}

// response is the API's envelope: sync (metadata), async (operation) or error.
type response struct {
	Type       string          `json:"type"`
	StatusCode int             `json:"status_code"`
	Operation  string          `json:"operation"`
	Error      string          `json:"error"`
	ErrorCode  int             `json:"error_code"`
	Metadata   json.RawMessage `json:"metadata"`
}

// ErrNotFound is the API's 404.
var ErrNotFound = errors.New("not found")

// raw sends a request and returns the answer's body, whatever its type.
func (c *Client) raw(ctx context.Context, method, path string, body io.Reader, header http.Header) ([]byte, http.Header, error) {
	target := c.base + path
	if c.project != "" && c.project != "default" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		target += sep + "project=" + url.QueryEscape(c.project)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		var r response
		message := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &r) == nil && r.Error != "" {
			message = r.Error
		}
		err := fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, message)
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return nil, nil, err
	}
	return data, resp.Header, nil
}

// call sends a JSON request and returns the envelope of a sync or async answer.
func (c *Client) call(ctx context.Context, method, path string, body any, header http.Header) (response, http.Header, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return response{}, nil, err
		}
		reader = bytes.NewReader(data)
		if header == nil {
			header = http.Header{}
		}
		header.Set("Content-Type", "application/json")
	}
	data, h, err := c.raw(ctx, method, path, reader, header)
	if err != nil {
		return response{}, nil, err
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return response{}, nil, fmt.Errorf("%s %s: not an API answer: %w", method, path, err)
	}
	if r.Type == "error" {
		return r, nil, fmt.Errorf("%s %s: %d %s", method, path, r.ErrorCode, r.Error)
	}
	return r, h, nil
}

// get decodes a sync answer's metadata into out.
func (c *Client) get(ctx context.Context, path string, out any) (http.Header, error) {
	r, h, err := c.call(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return h, json.Unmarshal(r.Metadata, out)
}

// operation is a background operation as it ends.
type operation struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	StatusCode int             `json:"status_code"`
	Err        string          `json:"err"`
	Metadata   json.RawMessage `json:"metadata"`
}

// do sends a request that may start a background operation, and waits for that to end; it
// returns the operation's metadata (a sync answer's, for one that ended at once).
func (c *Client) do(ctx context.Context, method, path string, body any, header http.Header) (json.RawMessage, error) {
	r, _, err := c.call(ctx, method, path, body, header)
	if err != nil {
		return nil, err
	}
	if r.Type != "async" {
		return r.Metadata, nil
	}
	if !strings.HasPrefix(r.Operation, "/1.0/operations/") {
		return nil, fmt.Errorf("%s %s: no operation in the answer", method, path)
	}
	wait := 60
	if deadline, ok := ctx.Deadline(); ok {
		wait = max(1, int(time.Until(deadline).Seconds()))
	}
	for {
		var op operation
		path := r.Operation + "/wait?timeout=" + fmt.Sprint(min(wait, 60))
		if _, err := c.get(ctx, path, &op); err != nil {
			return nil, err
		}
		switch {
		case op.StatusCode == 200:
			return op.Metadata, nil
		case op.StatusCode >= 400:
			return op.Metadata, fmt.Errorf("%s %s: %s: %s", method, strings.SplitN(path, "?", 2)[0], op.Status, op.Err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// Ping tells whether the server answers, and trusts the broker.
func (c *Client) Ping(ctx context.Context) error {
	var server struct {
		Auth string `json:"auth"`
	}
	if _, err := c.get(ctx, "/1.0", &server); err != nil {
		return err
	}
	if server.Auth != "trusted" {
		return fmt.Errorf("the server does not trust the broker's certificate (auth %q)", server.Auth)
	}
	return nil
}
