package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func aiCompletion(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
		"message": map[string]string{"content": content}, "finish_reason": "stop",
	}}})
}

func aiTestServer(t *testing.T, ai *AIClient, password string) (*httptest.Server, *Store) {
	t.Helper()
	_, st := newTestAPI(t, password)
	srv := httptest.NewServer((&API{store: st, password: password, ai: ai, indexHTML: []byte("test")}).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestAIParsePreviewAndCreate(t *testing.T) {
	var called atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fake-key" {
			t.Error("wrong OpenAI request path or authorization")
		}
		var body struct {
			Model          string            `json:"model"`
			Stream         bool              `json:"stream"`
			ResponseFormat map[string]string `json:"response_format"`
			Messages       []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Model != "test-model" || body.Stream || body.ResponseFormat["type"] != "json_object" || len(body.Messages) != 2 {
			t.Errorf("wrong completion request: %#v", body)
			return
		}
		var input struct {
			Text          string   `json:"text"`
			ReferenceDate string   `json:"reference_date"`
			Timezone      string   `json:"timezone"`
			Modules       []Module `json:"modules"`
			Members       []Member `json:"members"`
		}
		if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil {
			t.Error(err)
			return
		}
		if input.Text != "爸爸今天体检，妈妈明天交电费" || input.ReferenceDate != time.Now().Format(dateLayout) || input.Timezone != time.Local.String() || len(input.Modules) != 1 || input.Modules[0].Key != "calendar" || len(input.Members) != 4 {
			t.Errorf("incorrect extraction context: %#v", input)
		}
		if strings.Contains(body.Messages[1].Content, "password") || strings.Contains(body.Messages[1].Content, "created_at") {
			t.Error("unnecessary private data in model context")
		}
		// 系统提示词要带上本次真实枚举值，避免模型把 module_key 置空或向用户索要 reference_date。
		system := body.Messages[0].Content
		for _, want := range []string{"reference_date", "calendar（日历）", "1（爸爸）", "显式指定 module_key=calendar"} {
			if !strings.Contains(system, want) {
				t.Errorf("system prompt missing %q: %s", want, system)
			}
		}
		anchor, _ := time.Parse(dateLayout, input.ReferenceDate)
		aiCompletion(w, fmt.Sprintf(`{"drafts":[
			{"item":{"module_key":"calendar","title":"体检","occur_at":%q,"member_id":1,"payload":{}},"member_scope":"matched","questions":[]},
			{"item":{"module_key":"calendar","title":"交电费","occur_at":%q,"member_id":2,"payload":{}},"member_scope":"matched","questions":[]}
		],"questions":[]}`, input.ReferenceDate, anchor.AddDate(0, 0, 1).Format(dateLayout)))
	}))
	t.Cleanup(upstream.Close)
	ai, err := newAIClient(upstream.URL+"/v1/", "fake-key", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	srv, st := aiTestServer(t, ai, "test-token-123")
	headers := map[string]string{"Authorization": "Bearer test-token-123"}
	_, before, err := st.ListItems(context.Background(), ItemFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	code, env := call(t, srv, "POST", "/api/v1/items/parse", map[string]any{
		"text": "爸爸今天体检，妈妈明天交电费", "module_key": "calendar",
	}, headers)
	if code != 200 {
		t.Fatalf("parse: %d %#v", code, env)
	}
	drafts := dataMap(t, env)["drafts"].([]any)
	if len(drafts) != 2 || called.Load() != 1 {
		t.Fatalf("expected two drafts in one request: %#v", env)
	}
	_, after, _ := st.ListItems(context.Background(), ItemFilter{Limit: 200})
	if after != before {
		t.Fatal("parsing must not write items")
	}
	item := drafts[0].(map[string]any)["item"]
	code, env = call(t, srv, "POST", "/api/v1/items", item, headers)
	if code != 201 || dataMap(t, env)["title"] != "体检" {
		t.Fatalf("confirm creation: %d %#v", code, env)
	}
	_, after, _ = st.ListItems(context.Background(), ItemFilter{Limit: 200})
	if after != before+1 {
		t.Fatal("confirmation should create exactly one item")
	}
	code, env = call(t, srv, "GET", "/api/v1/bootstrap", nil, headers)
	data := dataMap(t, env)
	if code != 200 || data["ai_enabled"] != true || strings.Contains(fmt.Sprint(data), "fake-key") {
		t.Fatal("bootstrap must only expose AI availability")
	}
}

func TestAIParsePermissions(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		aiCompletion(w, `{"drafts":[],"questions":["请补充事项"]}`)
	}))
	defer upstream.Close()
	ai, _ := newAIClient(upstream.URL, "fake", "test")
	srv, st := aiTestServer(t, ai, "test-token-123")
	for _, role := range []string{"viewer", "member"} {
		if _, err := st.CreateUser(context.Background(), role, role, "test-password-123", role, nil); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"text": "今天体检"}
	code, _ := call(t, srv, "POST", "/api/v1/items/parse", body, nil)
	if code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	for _, role := range []string{"viewer", "member"} {
		browser := newBrowser(srv)
		_, login := apiCall(t, browser, srv, "POST", "/api/v1/auth/login", map[string]string{"username": role, "password": "test-password-123"}, "")
		csrf := dataMap(t, login)["csrf_token"].(string)
		code, _ = apiCall(t, browser, srv, "POST", "/api/v1/items/parse", body, "")
		if code != 403 {
			t.Errorf("missing CSRF: %d", code)
		}
		if role == "viewer" {
			code, _ = apiCall(t, browser, srv, "POST", "/api/v1/items/parse", body, csrf)
			if code != 403 {
				t.Errorf("viewer: %d", code)
			}
		} else {
			if calls.Load() != 0 {
				t.Fatal("unauthorized calls reached model")
			}
			code, _ = apiCall(t, browser, srv, "POST", "/api/v1/items/parse", body, csrf)
			if code != 200 {
				t.Errorf("member with CSRF: %d", code)
			}
		}
	}
	setup, _ := aiTestServer(t, ai, "")
	code, _ = call(t, setup, "POST", "/api/v1/items/parse", body, nil)
	if code != 403 || calls.Load() != 1 {
		t.Fatal("setup mode must not reach model")
	}
	unconfigured, _ := aiTestServer(t, nil, "test-token-123")
	code, env := call(t, unconfigured, "POST", "/api/v1/items/parse", body, map[string]string{"Authorization": "Bearer test-token-123"})
	if code != 503 || env["error"].(map[string]any)["code"] != "AI_NOT_CONFIGURED" {
		t.Fatalf("unconfigured: %d %#v", code, env)
	}
}

type aiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f aiRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAIErrorDiagnostics(t *testing.T) {
	_, st := newTestAPI(t, "test-token-123")
	ai, _ := newAIClient("http://localhost", "fake-secret", "test")
	ai.client.Transport = aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private upstream content fake-secret")), Header: make(http.Header)}, nil
	})
	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	req := httptest.NewRequest("POST", "/api/v1/items/parse", strings.NewReader(`{"text":"private family record"}`))
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, User{Role: "admin", Username: "admin"}))
	w := httptest.NewRecorder()
	(&API{store: st, ai: ai}).parseItems(w, req)
	if w.Code != 502 || !strings.Contains(logs.String(), "[AI] extraction failed:") || !strings.Contains(logs.String(), "HTTP 401") {
		t.Fatalf("missing server diagnostic: status=%d logs=%s", w.Code, logs.String())
	}
	for _, private := range []string{"fake-secret", "private upstream content", "private family record", "HTTP 401"} {
		if strings.Contains(w.Body.String(), private) {
			t.Errorf("client received private diagnostic: %s", private)
		}
		if private != "HTTP 401" && strings.Contains(logs.String(), private) {
			t.Errorf("logs contain private content: %s", private)
		}
	}
	for _, tc := range []struct{ content, want string }{
		{`{"drafts": "private family record"}`, "JSON type mismatch"},
		{`{"private family record":true}`, "unexpected JSON field"},
		{`{"drafts":`, "invalid JSON"},
		{"", "empty JSON content"},
	} {
		var result parseResult
		err := strictJSON([]byte(tc.content), &result)
		if err == nil {
			t.Fatal("expected invalid JSON")
		}
		diagnostic := aiJSONDiagnostic(err)
		if !strings.Contains(diagnostic, tc.want) || strings.Contains(diagnostic, "private family record") {
			t.Fatalf("unsafe or missing JSON diagnostic: %s", diagnostic)
		}
	}
}

// 线上真实故障：模型把日期写成 JSON 数字，解码报 type mismatch。
// 这个用例同时锁住三件事：诊断能指出字段、日志会打印模型原始输出、密钥不外泄。
func TestAIRawResponseLogging(t *testing.T) {
	const content = `{"drafts":[{"item":{"module_key":"calendar","title":"体检","occur_at":20261108},"member_scope":"unknown","questions":[]}],"questions":[]}`

	newClient := func(t *testing.T) *AIClient {
		t.Helper()
		ai, err := newAIClient("http://localhost", "fake-secret", "test-model")
		if err != nil {
			t.Fatal(err)
		}
		ai.client.Transport = aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"message": map[string]string{"content": content}, "finish_reason": "stop",
			}}})
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
		})
		return ai
	}
	run := func(t *testing.T) (error, string) {
		t.Helper()
		var logs strings.Builder
		previous := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(previous)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := newClient(t).extract(ctx, map[string]any{"text": "今天体检"})
		if err == nil {
			t.Fatal("期望解码失败")
		}
		return err, logs.String()
	}

	t.Run("默认打印模型原始输出", func(t *testing.T) {
		err, logs := run(t)
		if !strings.Contains(err.Error(), "drafts.item.occur_at") || !strings.Contains(err.Error(), "got number") {
			t.Fatalf("诊断未指出字段与类型: %v", err)
		}
		if !strings.Contains(logs, "[AI] raw content") || !strings.Contains(logs, `"occur_at":20261108`) {
			t.Fatalf("未打印模型原始输出: %s", logs)
		}
		if strings.Contains(logs, "fake-secret") {
			t.Fatalf("日志泄露密钥: %s", logs)
		}
		if strings.Count(strings.TrimRight(logs, "\n"), "\n") != 0 {
			t.Fatalf("原始输出应压成单行: %q", logs)
		}
	})

	t.Run("FW_AI_LOG_RAW=0 关闭打印", func(t *testing.T) {
		t.Setenv("FW_AI_LOG_RAW", "0")
		_, logs := run(t)
		if strings.Contains(logs, "[AI] raw") {
			t.Fatalf("关闭后仍打印: %s", logs)
		}
	})

	t.Run("按上限截断", func(t *testing.T) {
		t.Setenv("FW_AI_LOG_RAW", "20")
		_, logs := run(t)
		if !strings.Contains(logs, "(truncated)") || strings.Contains(logs, `"occur_at":20261108`) {
			t.Fatalf("未按上限截断: %s", logs)
		}
	})
}

func TestAIUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"bad JSON", 200, "not JSON", 502},
		{"rate limited", 429, "private key must never be returned", 502},
		{"upstream error", 500, "private upstream response", 502},
		{"oversized", 200, strings.Repeat("x", (128<<10)+1), 502},
		{"truncated", 200, `{"choices":[{"message":{"content":"{}"},"finish_reason":"length"}]}`, 502},
		{"bad content", 200, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`, 502},
		{"timeout", 0, "", 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ai, _ := newAIClient("http://localhost", "fake-secret", "test")
			ai.client.Transport = aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.status == 0 {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})
			srv, _ := aiTestServer(t, ai, "test-token-123")
			code, env := call(t, srv, "POST", "/api/v1/items/parse", map[string]string{"text": "今天体检"}, map[string]string{"Authorization": "Bearer test-token-123"})
			if code != tc.code || strings.Contains(fmt.Sprint(env), "private") || strings.Contains(fmt.Sprint(env), "fake-secret") {
				t.Fatalf("error: %d %#v", code, env)
			}
		})
	}
}

func TestUpstreamErrorDiagnostics(t *testing.T) {
	_, st := newTestAPI(t, "test-token-123")
	ai, _ := newAIClient("http://localhost", "fake-secret", "test-model")
	ai.client.Transport = aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"400004","message":"private model text fake-secret"}}`)), Header: make(http.Header)}, nil
	})
	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	req := httptest.NewRequest("POST", "/api/v1/items/parse", strings.NewReader(`{"text":"private family record"}`))
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, User{Role: "admin", Username: "admin"}))
	w := httptest.NewRecorder()
	(&API{store: st, ai: ai}).parseItems(w, req)
	if w.Code != 502 {
		t.Fatalf("status: %d %#v", w.Code, w.Body.String())
	}
	for _, want := range []string{"[AI] extraction failed:", "HTTP 400", "service code 400004", "model=test-model", "FW_AI_MODEL"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing diagnostic %q in logs: %s", want, logs.String())
		}
	}
	for _, private := range []string{"private model text", "private family record", "fake-secret"} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("logs contain private content: %s", private)
		}
		if strings.Contains(w.Body.String(), private) {
			t.Errorf("client received private diagnostic: %s", private)
		}
	}
}

func TestValidateAIDraft(t *testing.T) {
	modules := map[string]Module{}
	for _, m := range builtinModules() {
		if m.Adapter == "generic" {
			modules[m.Key] = m
		}
	}
	modules["money"] = Module{Key: "money", Schema: []Field{{Key: "amount", Type: "number", Required: true}}}
	members := []Member{{ID: 1, Name: "爸爸"}}
	for _, tc := range []struct {
		name string
		item string
		bad  bool
	}{
		{"valid", `{"module_key":"calendar","title":"体检","occur_at":"2026-11-07","member_id":1}`, false},
		{"unknown member and date", `{"module_key":"calendar","title":"体检"}`, false},
		{"unknown module", `{"title":"待分类"}`, false},
		{"forged module", `{"module_key":"users","title":"建账号"}`, true},
		{"forged member", `{"module_key":"calendar","title":"体检","member_id":999}`, true},
		{"invalid date", `{"module_key":"calendar","occur_at":"2026-02-30"}`, true},
		{"invalid end date", `{"module_key":"calendar","end_at":"明天"}`, true},
		{"invalid status", `{"module_key":"calendar","status":"pending"}`, true},
		{"invalid enum", `{"module_key":"todo","payload":{"priority":"high"}}`, true},
		{"unknown payload", `{"module_key":"calendar","payload":{"hospital":"X"}}`, true},
		{"payload without module", `{"payload":{"note":"X"}}`, true},
		{"number string", `{"module_key":"money","payload":{"amount":"80"}}`, true},
		{"number", `{"module_key":"money","payload":{"amount":80}}`, false},
		{"missing required", `{"module_key":"money","payload":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var draft itemDraft
			if err := json.Unmarshal([]byte(tc.item), &draft.Item); err != nil {
				t.Fatal(err)
			}
			err := validateDraft(&draft, modules, members)
			if (err != nil) != tc.bad {
				t.Fatalf("unexpected validation: %v", err)
			}
			if !tc.bad && draft.Item.MemberID == nil && (draft.MemberScope != "unknown" || len(draft.Questions) == 0) {
				t.Fatal("unknown member must require confirmation")
			}
		})
	}
	draft := itemDraft{Item: createItemReq{ModuleKey: "calendar", Title: "聚餐"}, MemberScope: "family"}
	if err := validateDraft(&draft, modules, members); err != nil || draft.MemberScope != "family" {
		t.Fatal("explicit family should remain family")
	}
	for _, q := range draft.Questions {
		if q.Field == "member_id" {
			t.Fatal("explicit family must not be treated as unknown")
		}
	}
	id := int64(1)
	for _, scope := range []string{"unknown", "family", "invalid"} {
		inconsistent := itemDraft{Item: createItemReq{ModuleKey: "calendar", Title: "体检", MemberID: &id}, MemberScope: scope}
		if err := validateDraft(&inconsistent, modules, members); err == nil {
			t.Errorf("accepted inconsistent member scope: %s", scope)
		}
	}
}

func TestAIOutputCannotEscapeModule(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aiCompletion(w, `{"drafts":[{"item":{"module_key":"todo","title":"越界草稿"},"member_scope":"family","questions":[]}],"questions":[]}`)
	}))
	defer upstream.Close()
	ai, _ := newAIClient(upstream.URL, "fake", "test")
	srv, _ := aiTestServer(t, ai, "test-token-123")
	code, env := call(t, srv, "POST", "/api/v1/items/parse", map[string]string{"text": "体检", "module_key": "calendar"}, map[string]string{"Authorization": "Bearer test-token-123"})
	if code != 502 || env["error"].(map[string]any)["code"] != "AI_INVALID_DRAFT" {
		t.Fatalf("model must stay within requested module: %d %#v", code, env)
	}
}

func TestAIClientDoesNotRedirect(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
	}))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	ai, _ := newAIClient(upstream.URL, "fake", "test")
	if _, err := ai.extract(context.Background(), map[string]string{"text": "test"}); err == nil || leaked.Load() != 0 {
		t.Fatal("redirects must not receive model input or credentials")
	}
}

func TestCreateItemChecksMemberAndModuleRole(t *testing.T) {
	srv, st := newTestAPI(t, "test-token-123")
	if _, err := st.CreateUser(context.Background(), "writer", "writer", "test-password-123", "member", nil); err != nil {
		t.Fatal(err)
	}
	m := Module{Key: "private", Name: "管理员模块", Adapter: "generic", RequiredRole: "admin", Enabled: true, ViewType: "list", Schema: coreSchema()}
	if err := st.CreateModule(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	browser := newBrowser(srv)
	_, login := apiCall(t, browser, srv, "POST", "/api/v1/auth/login", map[string]string{"username": "writer", "password": "test-password-123"}, "")
	csrf := dataMap(t, login)["csrf_token"].(string)
	code, _ := apiCall(t, browser, srv, "POST", "/api/v1/items", map[string]any{"module_key": "private", "title": "不允许"}, csrf)
	if code != 403 {
		t.Fatalf("private module create must be forbidden: %d", code)
	}
	code, _ = apiCall(t, browser, srv, "POST", "/api/v1/items", map[string]any{"module_key": "calendar", "title": "无效成员", "member_id": 9999}, csrf)
	if code != 400 {
		t.Fatalf("nonexistent member must be rejected: %d", code)
	}
}

func TestAIConfigAndLimits(t *testing.T) {
	if ai, err := newAIClient("not a URL", "", ""); err != nil || ai != nil {
		t.Fatal("missing key must disable AI")
	}
	for _, base := range []string{"bad", "file:///tmp/test", "https://user:pass@localhost", "https://localhost?key=secret"} {
		if _, err := newAIClient(base, "fake", "test"); err == nil {
			t.Errorf("accepted bad base URL: %s", base)
		}
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		aiCompletion(w, `{"drafts":[],"questions":["请补充事项"]}`)
	}))
	defer upstream.Close()
	ai, _ := newAIClient(upstream.URL, "fake", "test")
	srv, _ := aiTestServer(t, ai, "test-token-123")
	for _, body := range []map[string]string{
		{"text": " "}, {"text": strings.Repeat("字", 2001)}, {"text": "体检", "module_key": "users"}, {"text": "体检", "module_key": "missing"},
	} {
		code, _ := call(t, srv, "POST", "/api/v1/items/parse", body, map[string]string{"Authorization": "Bearer test-token-123"})
		if code != 400 {
			t.Errorf("bad input: %d", code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached upstream")
	}
	if err := strictJSON([]byte(`{"drafts":[],"unexpected":true}`), &parseResult{}); err == nil {
		t.Fatal("model unknown fields must be rejected")
	}
	if err := strictJSON([]byte(`{"drafts":[]} {}`), &parseResult{}); err == nil {
		t.Fatal("multiple JSON objects must be rejected")
	}
}
