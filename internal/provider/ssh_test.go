// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
)

type call struct {
	argv  []string
	stdin string
}

func fake(answer string, err error, calls *[]call) Runner {
	return func(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
		*calls = append(*calls, call{argv, string(stdin)})
		return []byte(answer), err
	}
}

var host = config.Host{Name: "ci-1", Address: "ci-1.example.org", MemoryMiB: 8000}

func sshConfig() config.SSH {
	return config.SSH{User: "berth", Port: 2222, KeyFile: "/etc/forgejo-berth/ssh_key",
		KnownHostsFile: "/etc/forgejo-berth/known_hosts", TimeoutSeconds: 20}
}

func TestStatusAsksTheHostWithItsKeyChecked(t *testing.T) {
	var calls []call
	s := NewSSH(sshConfig(), fake(`{"slots": [
		{"container": "a", "state": "ready", "assignment": "", "last": {}},
		{"container": "b", "state": "resetting", "assignment": "",
		 "last": {"assignment": "ci-1-b-1-7", "task": 555, "peak_mib": 2100, "seconds": 300.5}},
		{"state": "ready"}, "junk"]}`, nil, &calls))
	slots, err := s.Status(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	want := []Slot{
		{Host: "ci-1", Container: "a", State: "ready"},
		{Host: "ci-1", Container: "b", State: "resetting", Finished: "ci-1-b-1-7", Task: 555, PeakMiB: 2100, Seconds: 300.5},
	}
	if !reflect.DeepEqual(slots, want) {
		t.Fatalf("slots %+v", slots)
	}
	argv := calls[0].argv
	for _, opt := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "IdentitiesOnly=yes",
		"UserKnownHostsFile=/etc/forgejo-berth/known_hosts"} {
		if !slices.Contains(argv, opt) {
			t.Errorf("argv lacks %s: %v", opt, argv)
		}
	}
	if argv[slices.Index(argv, "-p")+1] != "2222" || argv[slices.Index(argv, "-l")+1] != "berth" ||
		!slices.Equal(argv[len(argv)-3:], []string{"--", "ci-1.example.org", "status"}) {
		t.Fatalf("argv %v", argv)
	}
}

func TestAssignSendsTheHandoffOnStandardInput(t *testing.T) {
	var calls []call
	s := NewSSH(sshConfig(), fake("ci-1-a-1-7\n", nil, &calls))
	h := Handoff{Assignment: "ci-1-a-1-7", UUID: "u", Token: "secret", Labels: []string{"docker"}, Class: "shared", UnusedMinutes: 15}
	if err := s.Assign(context.Background(), host, "a", h); err != nil {
		t.Fatal(err)
	}
	if calls[0].argv[len(calls[0].argv)-1] != "assign a" || !strings.Contains(calls[0].stdin, `"token":"secret"`) {
		t.Fatalf("call %+v", calls[0])
	}
	if strings.Contains(strings.Join(calls[0].argv, " "), "secret") {
		t.Fatal("the token is on the command line")
	}
	if err := s.Assign(context.Background(), host, "a; id", h); err == nil {
		t.Fatal("assigned to a container named like a command")
	}
}

func TestReleaseAndErrors(t *testing.T) {
	var calls []call
	s := NewSSH(sshConfig(), fake("", errors.New("exit status 1: b does not hold x"), &calls))
	err := s.Release(context.Background(), host, "b", "ci-1-b-1-7")
	if err == nil || !strings.Contains(err.Error(), "does not hold") || calls[0].argv[len(calls[0].argv)-1] != "release b ci-1-b-1-7" {
		t.Fatalf("release %v, %+v", err, calls)
	}
	if _, err := ParseStatus("ci-1", []byte("not json")); err == nil {
		t.Fatal("took a status that is no JSON")
	}
}

func TestAHandoffNeverPrintsItsToken(t *testing.T) {
	h := Handoff{Assignment: "a-1", Token: "secret"}
	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, h), "secret") {
			t.Errorf("%s prints the token", format)
		}
	}
}
