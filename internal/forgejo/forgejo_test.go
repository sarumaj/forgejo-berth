// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/broker"
)

// api serves the paths given, recording each request as "METHOD path?query token".
func api(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) (*Client, *[]string) {
	t.Helper()
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization")))
		if h, ok := routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return New(server.URL+"/", "admin", map[string]string{"alice": "alice-token"}, server.Client()), &seen
}

func reply(v any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(v) }
}

func TestWaitingJobs(t *testing.T) {
	c, seen := api(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/admin/runners/jobs": reply(map[string]any{"jobs": []map[string]any{
			{"id": 7, "run_id": 70, "repo_id": 3, "name": "build (1)", "runs_on": []string{"docker"}, "status": "waiting"},
			{"id": 8, "run_id": 80, "repo_id": 3, "name": "lint", "runs_on": []string{"docker"}, "status": "running"},
			{"id": 9, "run_id": 90, "repo_id": 4, "name": "gone", "runs_on": []string{"docker"}, "status": "waiting"},
		}}),
		"GET /api/v1/repositories/3": reply(map[string]any{"full_name": "alice/app", "owner": map[string]any{"login": "alice"}}),
	})
	jobs, err := c.WaitingJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := broker.Job{ID: 7, RunID: 70, RepoID: 3, Owner: "alice", Repo: "alice/app", Name: "build (1)", RunsOn: []string{"docker"}}
	if len(jobs) != 1 || fmt.Sprint(jobs[0]) != fmt.Sprint(want) {
		t.Fatalf("jobs %+v", jobs)
	}
	if (*seen)[0] != "GET /api/v1/admin/runners/jobs?page=1&limit=50 token admin" {
		t.Fatalf("requests %v", *seen)
	}
}

func TestPagesAreReadUntilTheyEnd(t *testing.T) {
	calls := 0
	c, _ := api(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/admin/actions/runners": func(w http.ResponseWriter, r *http.Request) {
			calls++
			var list []map[string]any
			if r.URL.Query().Get("page") == "1" {
				for i := range page {
					list = append(list, map[string]any{"id": i + 1, "name": fmt.Sprint("r", i)})
				}
			} else if r.URL.Query().Get("page") == "2" {
				list = append(list, map[string]any{"id": 999, "name": "last"}, map[string]any{"id": 5, "owner_id": 1})
			}
			_ = json.NewEncoder(w).Encode(list)
		},
		"GET /api/v1/user/actions/runners": reply(map[string]any{"runners": []map[string]any{{"id": 4, "name": "berth-x"}}}),
	})
	runners, err := c.Runners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(runners) != page+2 {
		t.Fatalf("%d calls, %d runners", calls, len(runners))
	}
	if last := runners[len(runners)-1]; last != (broker.Runner{Scope: "user:alice", ID: 4, Name: "berth-x"}) {
		t.Fatalf("last %+v", last)
	}
}

func TestRegisterAndDelete(t *testing.T) {
	var body map[string]any
	c, seen := api(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/user/actions/runners": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			reply(map[string]any{"id": 12, "uuid": "u-1", "token": "t-1"})(w, r)
		},
		"DELETE /api/v1/admin/actions/runners/5": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	got, err := c.Register(context.Background(), "user:alice", "berth-a")
	if err != nil || got != (broker.Registered{ID: 12, UUID: "u-1", Token: "t-1"}) {
		t.Fatalf("registered %+v, %v", got, err)
	}
	if body["name"] != "berth-a" || body["ephemeral"] != true || !strings.HasSuffix((*seen)[0], "token alice-token") {
		t.Fatalf("body %v, requests %v", body, *seen)
	}
	if err := c.Delete(context.Background(), "global", 5); err != nil {
		t.Fatal(err)
	}
	// Forgejo removed it already after its job: fine.
	if err := c.Delete(context.Background(), "global", 6); err != nil {
		t.Fatalf("a runner gone already: %v", err)
	}
	if _, err := c.Register(context.Background(), "user:mallory", "x"); err == nil {
		t.Fatal("registered for an owner without a token")
	}
}

func TestARegistrationWithoutATokenIsAnError(t *testing.T) {
	c, _ := api(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/admin/actions/runners": reply(map[string]any{"id": 1, "uuid": "u"}),
	})
	if _, err := c.Register(context.Background(), "global", "berth-a"); err == nil {
		t.Fatal("took a registration without a token")
	}
}

func TestTaskJob(t *testing.T) {
	c, _ := api(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/repos/alice/app/actions/runs/70/jobs": reply(map[string]any{"jobs": []map[string]any{
			{"id": 6, "task_id": 555, "status": "success"},
			{"id": 7, "task_id": 555, "status": "completed", "conclusion": "failure"},
		}}),
	})
	candidates := []broker.Job{
		{ID: 9, RunID: 90, Repo: "alice/gone"},
		{ID: 7, RunID: 70, Repo: "alice/app"},
	}
	job, end, found, err := c.TaskJob(context.Background(), 555, candidates)
	if err != nil || !found || job.ID != 7 || end != "failure" {
		t.Fatalf("TaskJob = %v %s %v %v", job, end, found, err)
	}
	if _, _, _, err := c.TaskJob(context.Background(), 555, candidates[:1]); err == nil {
		t.Fatal("a run that cannot be read is not an error")
	}
	if _, _, found, err := c.TaskJob(context.Background(), 1, candidates[1:]); err != nil || found {
		t.Fatal("found a task of none of the jobs")
	}
}

func TestAnErrorNamesNoToken(t *testing.T) {
	c, _ := api(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/admin/runners/jobs": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusForbidden)
		},
	})
	_, err := c.WaitingJobs(context.Background())
	if err == nil || strings.Contains(err.Error(), "admin") && strings.Contains(err.Error(), "token admin") {
		t.Fatalf("error %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error %v", err)
	}
}
