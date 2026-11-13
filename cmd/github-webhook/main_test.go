package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostCommandEnvAvoidsDockerSocket(t *testing.T) {
	got := strings.Join(commandEnv(true), "\n")
	if strings.Contains(got, "container=") || strings.Contains(got, "DOCKER_HOST=") || strings.Contains(got, "COMPOSE=") {
		t.Fatalf("host env still overrides the login shell: %s", got)
	}
	if !strings.Contains(got, "HOME=/root") || !strings.Contains(got, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("env = %s", got)
	}
}

func TestDeployCommandUsesHostNamespaces(t *testing.T) {
	cmd := deployCommand(context.Background(), true, "/root/family-workspace", "make", "up")
	got := strings.Join(cmd.Args, "\x00")
	want := strings.Join([]string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "--",
		"bash", "-c", hostCommandScript, "bash",
		"make", "-C", "/root/family-workspace", "up",
	}, "\x00")
	if got != want {
		t.Fatalf("got %q", strings.Join(cmd.Args, " "))
	}
}

func TestHealth(t *testing.T) {
	h := &hook{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
		t.Fatalf("health response %d %q", w.Code, w.Body.String())
	}
}

func TestWebhook(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	h := &hook{secret: secret, repository: "owner/repo", branch: "main", queue: make(chan struct{}, 1)}
	valid := `{"ref":"refs/heads/main","repository":{"full_name":"owner/repo"}}`
	cases := []struct {
		name, event, body, signature, id string
		want                             int
	}{
		{"push", "push", valid, "valid", "one", http.StatusAccepted},
		{"duplicate", "push", valid, "valid", "one", http.StatusAccepted},
		{"bad signature", "push", valid, "bad", "two", http.StatusUnauthorized},
		{"wrong branch", "push", `{"ref":"refs/heads/other","repository":{"full_name":"owner/repo"}}`, "valid", "three", http.StatusNoContent},
		{"wrong repository", "push", `{"ref":"refs/heads/main","repository":{"full_name":"other/repo"}}`, "valid", "four", http.StatusNoContent},
		{"deleted", "push", `{"ref":"refs/heads/main","deleted":true,"repository":{"full_name":"owner/repo"}}`, "valid", "five", http.StatusNoContent},
		{"malformed", "push", `{`, "valid", "six", http.StatusBadRequest},
		{"missing delivery", "push", valid, "valid", "", http.StatusBadRequest},
		{"ping", "ping", `{}`, "valid", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(tc.body))
			r.Header.Set("X-GitHub-Event", tc.event)
			r.Header.Set("X-GitHub-Delivery", tc.id)
			mac := hmac.New(sha256.New, secret)
			mac.Write([]byte(tc.body))
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if tc.signature == "bad" {
				sig = "sha256=00"
			}
			r.Header.Set("X-Hub-Signature-256", sig)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
	if len(h.queue) != 1 {
		t.Fatalf("expected one queued deployment, got %d", len(h.queue))
	}
}

func TestDeploy(t *testing.T) {
	for _, tc := range []struct {
		name, branch, status, head, remote string
		fail                               bool
	}{
		{"success", "main", "", "abc", "abc", false},
		{"wrong branch", "other", "", "abc", "abc", true},
		{"dirty", "main", " M file", "abc", "abc", true},
		{"ahead", "main", "", "local", "remote", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			made := false
			run := func(_ context.Context, name string, args ...string) (string, error) {
				if name == "make" {
					made = true
					return "", nil
				}
				switch args[0] {
				case "branch":
					return tc.branch, nil
				case "status":
					return tc.status, nil
				case "rev-parse":
					if args[1] == "HEAD" {
						return tc.head, nil
					}
					return tc.remote, nil
				}
				return "", nil
			}
			err := deploy(context.Background(), run, "main")
			if (err != nil) != tc.fail {
				t.Fatalf("error %v", err)
			}
			if made == tc.fail {
				t.Fatalf("make called=%v, failed=%v", made, tc.fail)
			}
		})
	}
}
