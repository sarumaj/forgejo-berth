// SPDX-License-Identifier: GPL-3.0-or-later

// Package docker is the docker provider: every host is a Docker Engine (or Podman's
// Docker-compatible API), every slot a runner container made for one job and removed after
// it, with a Docker of its own beside it for the job's containers. The slots' reset cycles
// run in the broker (slot.Local). The Engine API is spoken with net/http alone.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
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

// maxBody is the most read of an answer that is not a stream: an inspect is a few KiB.
const maxBody = 4 << 20

// Engine is a client of the Docker Engine API. Paths carry no API version: an engine
// answers an unversioned request with its own version's shapes, and the fields used here
// are the same in every version since 1.24, Podman's included.
type Engine struct {
	client *http.Client
	base   string
}

// ErrNotFound is the engine's 404: no such container, image or volume.
var ErrNotFound = errors.New("not found")

// NewEngine makes a client of the engine at address: unix:///path, or tcp://host:port with
// TLS (the server checked against the CA, the broker's client certificate).
func NewEngine(address string, t config.TLS) (*Engine, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	e := &Engine{client: &http.Client{Transport: transport}}
	switch u.Scheme {
	case "unix":
		path := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
		e.base = "http://docker"
	case "tcp":
		tc, err := provider.TLSConfig(t)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tc
		e.base = "https://" + u.Host
	default:
		return nil, fmt.Errorf("%s: unix:///path or tcp://host:port", address)
	}
	return e, nil
}

// request sends a request; body is JSON unless it is an io.Reader (then sent as a tar).
func (e *Engine) request(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case io.Reader:
		reader, contentType = b, "application/x-tar"
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		reader, contentType = bytes.NewReader(data), "application/json"
	}
	target := e.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer resp.Body.Close()
	var answer struct {
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(data, &answer) != nil || answer.Message == "" {
		answer.Message = strings.TrimSpace(string(data))
	}
	err = fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, answer.Message)
	if resp.StatusCode == http.StatusNotFound {
		err = fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return nil, err
}

// call sends a request and decodes its JSON answer into out (nil: dropped).
func (e *Engine) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := e.request(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil || out == nil || len(data) == 0 {
		return err
	}
	return json.Unmarshal(data, out)
}

// Ping tells whether the engine answers.
func (e *Engine) Ping(ctx context.Context) error {
	return e.call(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// Image returns the image's configured user (uid[:gid], a name, or "").
func (e *Engine) Image(ctx context.Context, ref string) (string, error) {
	var image struct {
		Config struct {
			User string `json:"User"`
		} `json:"Config"`
	}
	err := e.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &image)
	return image.Config.User, err
}

// Pull pulls an image; an error in its progress stream is the pull's.
func (e *Engine) Pull(ctx context.Context, ref string) error {
	resp, err := e.request(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for {
		var progress struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&progress); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
		if progress.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, progress.Error)
		}
	}
}

// Mount is a volume or a bind mount of a container.
type Mount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	Target string `json:"Target"`
}

// Spec is the part of a container's creation the provider sets.
type Spec struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig is the part of a container's host configuration the provider sets.
type HostConfig struct {
	Privileged  bool    `json:"Privileged,omitempty"`
	Init        bool    `json:"Init,omitempty"`
	NetworkMode string  `json:"NetworkMode,omitempty"`
	Mounts      []Mount `json:"Mounts,omitempty"`
}

// Create makes a container of that name.
func (e *Engine) Create(ctx context.Context, name string, spec Spec) error {
	return e.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, spec, nil)
}

// Start starts a container (one running already is not an error).
func (e *Engine) Start(ctx context.Context, name string) error {
	return e.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil, nil)
}

// Kill sends a container's first process a signal.
func (e *Engine) Kill(ctx context.Context, name, signal string) error {
	return e.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/kill",
		url.Values{"signal": {signal}}, nil, nil)
}

// Remove removes a container, running or not, with its anonymous volumes; one that does
// not exist is not an error.
func (e *Engine) Remove(ctx context.Context, name string) error {
	err := e.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name),
		url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Running tells whether the container exists and runs.
func (e *Engine) Running(ctx context.Context, name string) (bool, error) {
	var c struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	err := e.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &c)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return c.State.Running, err
}

// Exists tells whether a path exists in the container.
func (e *Engine) Exists(ctx context.Context, name, path string) (bool, error) {
	resp, err := e.request(ctx, http.MethodHead, "/containers/"+url.PathEscape(name)+"/archive",
		url.Values{"path": {path}}, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// Upload unpacks a tar archive into the container at dir (it need not run).
func (e *Engine) Upload(ctx context.Context, name, dir string, archive []byte) error {
	return e.call(ctx, http.MethodPut, "/containers/"+url.PathEscape(name)+"/archive",
		url.Values{"path": {dir}}, bytes.NewReader(archive), nil)
}

// Logs returns the container's standard output and error, their last lines (0: all).
func (e *Engine) Logs(ctx context.Context, name string, lines int) (string, error) {
	tail := "all"
	if lines > 0 {
		tail = fmt.Sprint(lines)
	}
	resp, err := e.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/logs",
		url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {tail}}, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	err = demux(io.LimitReader(resp.Body, 64<<20), &out, &out)
	return out.String(), err
}

// demux splits the engine's multiplexed stream of a container without a terminal: frames of
// an 8-byte header (the stream, 0 0 0, the length) and the data. A stream that does not start
// with such a header is raw output.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	br := bufio.NewReader(r)
	first, err := br.Peek(8)
	if len(first) == 0 {
		return nil
	}
	if err != nil || first[0] > 2 || first[1] != 0 || first[2] != 0 || first[3] != 0 {
		_, err := io.Copy(stdout, br)
		return err
	}
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, header); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		w := stdout
		if header[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, br, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return err
		}
	}
}

// Exec runs a command in a running container and returns its standard output and its exit
// code.
func (e *Engine) Exec(ctx context.Context, name string, cmd ...string) (string, int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := e.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", nil,
		map[string]any{"AttachStdout": true, "AttachStderr": true, "Cmd": cmd}, &created)
	if err != nil {
		return "", 0, err
	}
	resp, err := e.request(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil,
		map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return "", 0, err
	}
	var stdout bytes.Buffer
	err = demux(io.LimitReader(resp.Body, 16<<20), &stdout, io.Discard)
	resp.Body.Close()
	if err != nil {
		return "", 0, err
	}
	var result struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := e.call(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &result); err != nil {
		return "", 0, err
	}
	return stdout.String(), result.ExitCode, nil
}

// CreateVolume makes a named volume.
func (e *Engine) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return e.call(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// RemoveVolume removes a named volume; one that does not exist is not an error.
func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	err := e.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// MaxUsage is the stats API's memory high-water mark of a container, where its engine
// reports one (cgroup v1; 0 otherwise).
func (e *Engine) MaxUsage(ctx context.Context, name string) (int64, error) {
	var stats struct {
		MemoryStats struct {
			MaxUsage int64 `json:"max_usage"`
		} `json:"memory_stats"`
	}
	err := e.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/stats",
		url.Values{"stream": {"false"}}, nil, &stats)
	return stats.MemoryStats.MaxUsage, err
}
