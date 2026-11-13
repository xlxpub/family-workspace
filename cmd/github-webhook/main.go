// github-webhook runs on the deployment host, independently of the application container.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxPayload = 1 << 20

type hook struct {
	secret             []byte
	repository, branch string
	queue              chan struct{}
	mu                 sync.Mutex
	seen               map[string]struct{}
	order              []string
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if r.URL.Path != "/webhooks/github" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayload))
	if err != nil {
		http.Error(w, "invalid or oversized body", http.StatusBadRequest)
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, "sha256=") {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write(body)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-GitHub-Event") == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "push" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var p struct {
		Ref        string `json:"ref"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if json.Unmarshal(body, &p) != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.Repository.FullName != h.repository || p.Ref != "refs/heads/"+h.branch || p.Deleted {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.Header.Get("X-GitHub-Delivery")
	if id == "" || len(id) > 128 {
		http.Error(w, "invalid delivery id", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	if _, ok := h.seen[id]; ok {
		h.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if h.seen == nil {
		h.seen = make(map[string]struct{})
	}
	h.seen[id] = struct{}{}
	h.order = append(h.order, id)
	if len(h.order) > 1024 {
		delete(h.seen, h.order[0])
		h.order = h.order[1:]
	}
	select {
	case h.queue <- struct{}{}:
	default:
	} // A queued run fetches the latest branch, coalescing bursts.
	h.mu.Unlock()
	log.Printf("accepted delivery %s for %s branch %s", id, h.repository, h.branch)
	w.WriteHeader(http.StatusAccepted)
}

type commandRunner func(context.Context, string, ...string) (string, error)

// 先加载宿主机登录配置，让 DOCKER_HOST 与手动执行 make up 一致。
const hostCommandScript = `source /etc/profile >/dev/null || true
if [ -z "${DOCKER_HOST-}" ] && [ -S /run/podman/podman.sock ] && [ ! -S /var/run/docker.sock ]; then
  export DOCKER_HOST=unix:///run/podman/podman.sock
fi
exec "$@"`

func commandEnv(nsenter bool) []string {
	if !nsenter {
		return append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	// 不继承容器环境。Podman 注入的 container=podman 会让 compose 去连
	// 宿主机上不存在的 /var/run/docker.sock。DOCKER_HOST 交给登录配置设置。
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"USER=root",
		"LOGNAME=root",
		"SHELL=/bin/bash",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func deployCommand(ctx context.Context, nsenter bool, dir, name string, args ...string) *exec.Cmd {
	if nsenter {
		argv := []string{"-t", "1", "-m", "-u", "-i", "-n", "--", "bash", "-c", hostCommandScript, "bash", name, "-C", dir}
		argv = append(argv, args...)
		cmd := exec.CommandContext(ctx, "nsenter", argv...)
		cmd.Env = commandEnv(true)
		return cmd
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = commandEnv(false)
	return cmd
}

func deploy(ctx context.Context, run commandRunner, branch string) error {
	if _, err := run(ctx, "git", "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid branch: %w", err)
	}
	current, err := run(ctx, "git", "branch", "--show-current")
	if err != nil {
		return err
	}
	if strings.TrimSpace(current) != branch {
		return fmt.Errorf("checkout is not on configured branch %s", branch)
	}
	status, err := run(ctx, "git", "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("tracked working tree has local changes")
	}
	if _, err = run(ctx, "git", "fetch", "--no-tags", "origin"); err != nil {
		return err
	}
	remote := "refs/remotes/origin/" + branch
	if _, err = run(ctx, "git", "merge", "--ff-only", remote); err != nil {
		return err
	}
	localSHA, err := run(ctx, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remoteSHA, err := run(ctx, "git", "rev-parse", remote)
	if err != nil {
		return err
	}
	if strings.TrimSpace(localSHA) != strings.TrimSpace(remoteSHA) {
		return errors.New("local HEAD differs from remote branch")
	}
	log.Printf("deploying commit %s", strings.TrimSpace(localSHA))
	_, err = run(ctx, "make", "up")
	return err
}

func main() {
	secret, repo, branch, dir := os.Getenv("FW_WEBHOOK_SECRET"), os.Getenv("FW_WEBHOOK_REPOSITORY"), os.Getenv("FW_WEBHOOK_BRANCH"), os.Getenv("FW_WEBHOOK_DIR")
	if len(secret) < 32 || repo == "" || branch == "" || !filepath.IsAbs(dir) {
		log.Fatal("set FW_WEBHOOK_SECRET (at least 32 bytes), FW_WEBHOOK_REPOSITORY, FW_WEBHOOK_BRANCH and absolute FW_WEBHOOK_DIR")
	}
	addr := os.Getenv("FW_WEBHOOK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8091"
	}
	h := &hook{secret: []byte(secret), repository: repo, branch: branch, queue: make(chan struct{}, 1)}
	nsenter := os.Getenv("FW_WEBHOOK_NSENTER") == "1"
	run := func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := deployCommand(ctx, nsenter, dir, name, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	go func() {
		for range h.queue {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			err := deploy(ctx, run, branch)
			cancel()
			if err != nil {
				log.Printf("deployment failed: %v", err)
			} else {
				log.Print("deployment completed")
			}
		}
	}()
	server := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}
	log.Printf("GitHub webhook listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
