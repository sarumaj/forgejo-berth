// SPDX-License-Identifier: GPL-3.0-or-later

// Package forgejo is the broker's client of the Forgejo API: the waiting jobs, ephemeral
// runners at the global and the trusted owners' scopes, and which job a task was.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/broker"
)

const (
	// page is the size of a page of a list, pages the most pages read.
	page  = 50
	pages = 40
	// maxBody is the most of an answer read.
	maxBody = 16 << 20
)

// HTTPError is an answer with an error status.
type HTTPError struct {
	Method, Path string
	Status       int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// Client talks to one Forgejo instance with the admin token and the owners' tokens.
type Client struct {
	base   string
	admin  string
	owners map[string]string
	http   *http.Client
	mu     sync.Mutex
	repos  map[int64][2]string
	log    *slog.Logger
}

// WithLog sets the logger of what the client skips (a job whose repository cannot be read).
func (c *Client) WithLog(log *slog.Logger) *Client {
	c.log = log
	return c
}

// New makes a client of the instance at baseURL; owners maps a trusted owner to a token of
// theirs.
func New(baseURL, adminToken string, owners map[string]string, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		base:   strings.TrimRight(baseURL, "/") + "/api/v1",
		admin:  adminToken,
		owners: owners,
		http:   client,
		repos:  map[int64][2]string{},
		log:    slog.New(slog.DiscardHandler),
	}
}

func (c *Client) call(ctx context.Context, method, path, token string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL holds no secret; the error names it, never the token.
		return nil, fmt.Errorf("%s %s: %w", method, path, unwrapURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return nil, &HTTPError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s %s: the answer is no JSON", method, path)
	}
	return data, nil
}

func unwrapURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// records returns the objects of an answer: a list of them, or the list under key.
func records(data json.RawMessage, key string) []map[string]any {
	var list []any
	if err := json.Unmarshal(data, &list); err != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil || object[key] == nil {
			return nil
		}
		if json.Unmarshal(object[key], &list) != nil {
			return nil
		}
	}
	var out []map[string]any
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// pagesOf reads a list the API may page, each object once.
func (c *Client) pagesOf(ctx context.Context, path, token, key string) ([]map[string]any, error) {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	seen := map[string]bool{}
	var found []map[string]any
	for p := 1; p <= pages; p++ {
		data, err := c.call(ctx, http.MethodGet, fmt.Sprintf("%s%spage=%d&limit=%d", path, separator, p, page), token, nil)
		if err != nil {
			return nil, err
		}
		list := records(data, key)
		fresh := 0
		for _, r := range list {
			id := fmt.Sprint(r["id"])
			if !seen[id] {
				seen[id] = true
				found = append(found, r)
				fresh++
			}
		}
		// Fewer than a page, or nothing new (the endpoint does not page): the end.
		if len(list) < page || fresh == 0 {
			break
		}
	}
	return found, nil
}

// number reads an id of an answer (0: none or not a number).
func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (c *Client) scope(scope string) (string, string, error) {
	if scope == broker.GlobalScope {
		return "/admin/actions/runners", c.admin, nil
	}
	owner, ok := strings.CutPrefix(scope, "user:")
	token := c.owners[owner]
	if !ok || token == "" {
		return "", "", fmt.Errorf("no token for the scope %s", scope)
	}
	return "/user/actions/runners", token, nil
}

// repo returns the repository's owner and full name (asked once).
func (c *Client) repo(ctx context.Context, id int64) (string, string, error) {
	c.mu.Lock()
	known, ok := c.repos[id]
	c.mu.Unlock()
	if ok {
		return known[0], known[1], nil
	}
	data, err := c.call(ctx, http.MethodGet, fmt.Sprintf("/repositories/%d", id), c.admin, nil)
	if err != nil {
		return "", "", err
	}
	var r struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.FullName == "" || r.Owner.Login == "" {
		return "", "", fmt.Errorf("repository %d: no owner or name", id)
	}
	c.mu.Lock()
	c.repos[id] = [2]string{r.Owner.Login, r.FullName}
	c.mu.Unlock()
	return r.Owner.Login, r.FullName, nil
}

// WaitingJobs returns the jobs waiting for a runner all over the instance (the search lists
// running ones too). A job whose repository cannot be read is skipped.
func (c *Client) WaitingJobs(ctx context.Context) ([]broker.Job, error) {
	raw, err := c.pagesOf(ctx, "/admin/runners/jobs", c.admin, "jobs")
	if err != nil {
		return nil, err
	}
	var jobs []broker.Job
	for _, r := range raw {
		if text(r["status"]) != "waiting" {
			continue
		}
		id, repoID := number(r["id"]), number(r["repo_id"])
		name, okName := r["name"].(string)
		if id == 0 || repoID == 0 || !okName {
			c.log.Warn("a waiting job out of shape is skipped", "job", r["id"])
			continue
		}
		owner, repo, err := c.repo(ctx, repoID)
		if err != nil {
			c.log.Warn("a waiting job is skipped", "job", id, "error", err)
			continue
		}
		var labels []string
		if list, ok := r["runs_on"].([]any); ok {
			for _, l := range list {
				labels = append(labels, fmt.Sprint(l))
			}
		}
		jobs = append(jobs, broker.Job{
			ID: id, RunID: number(r["run_id"]), RepoID: repoID, Owner: owner, Repo: repo,
			Name: name, RunsOn: labels,
		})
	}
	return jobs, nil
}

// Register registers an ephemeral runner of the scope.
func (c *Client) Register(ctx context.Context, scope, name string) (broker.Registered, error) {
	path, token, err := c.scope(scope)
	if err != nil {
		return broker.Registered{}, err
	}
	data, err := c.call(ctx, http.MethodPost, path, token, map[string]any{"name": name, "ephemeral": true})
	if err != nil {
		return broker.Registered{}, err
	}
	var r struct {
		ID    int64  `json:"id"`
		UUID  string `json:"uuid"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.ID == 0 || r.UUID == "" || r.Token == "" {
		return broker.Registered{}, fmt.Errorf("the runner %s was registered without id, uuid or token", name)
	}
	return broker.Registered{ID: r.ID, UUID: r.UUID, Token: r.Token}, nil
}

// Delete removes a runner; one Forgejo removed already (after its job) is fine.
func (c *Client) Delete(ctx context.Context, scope string, id int64) error {
	path, token, err := c.scope(scope)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", path, id), token, nil)
	var h *HTTPError
	if errors.As(err, &h) && h.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// Runners lists the runners of the broker's scopes; of the global ones only those of no
// owner or repository.
func (c *Client) Runners(ctx context.Context) ([]broker.Runner, error) {
	scopes := []string{broker.GlobalScope}
	for owner := range c.owners {
		scopes = append(scopes, "user:"+owner)
	}
	var found []broker.Runner
	for _, scope := range scopes {
		path, token, err := c.scope(scope)
		if err != nil {
			return nil, err
		}
		list, err := c.pagesOf(ctx, path, token, "runners")
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			if scope == broker.GlobalScope && (number(r["owner_id"]) != 0 || number(r["repo_id"]) != 0) {
				continue
			}
			if id := number(r["id"]); id != 0 {
				found = append(found, broker.Runner{Scope: scope, ID: id, Name: text(r["name"])})
			}
		}
	}
	return found, nil
}

// TaskJob finds the candidate whose task the runner ran and how it ended, each looked up
// among its run's jobs; a run deleted meanwhile is skipped, an error when none could be read.
func (c *Client) TaskJob(ctx context.Context, task int64, candidates []broker.Job) (broker.Job, string, bool, error) {
	var failed error
	for _, job := range candidates {
		path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", job.Repo, job.RunID)
		entries, err := c.pagesOf(ctx, path, c.admin, "jobs")
		if err != nil {
			failed = err
			continue
		}
		for _, e := range entries {
			if number(e["id"]) == job.ID && number(e["task_id"]) == task {
				end := text(e["conclusion"])
				if end == "" {
					end = text(e["status"])
				}
				return job, end, true, nil
			}
		}
	}
	if failed != nil {
		return broker.Job{}, "", false, failed
	}
	return broker.Job{}, "", false, nil
}
