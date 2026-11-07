package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
)

func newBrowser(srv *httptest.Server) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return &http.Client{Jar: jar}
}

func apiCall(t *testing.T, c *http.Client, srv *httptest.Server, method, path string, body any, csrf string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-FW-CSRF", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("解析响应 %s %s: %v", method, path, err)
	}
	return resp.StatusCode, env
}

// 完整链路：安装 → 登录 → 会话/CSRF → 模块级角色可见性 → 三种角色权限 → 用户管理 → 改密 → 登出。
func TestUserModuleAndAuthFlow(t *testing.T) {
	srv, _ := newTestAPI(t, "")

	// ① 初始状态：需要安装
	code, env := call(t, srv, "GET", "/api/v1/auth/status", nil, nil)
	if code != 200 || dataMap(t, env)["setup_required"] != true {
		t.Fatalf("应处于安装模式: %d %#v", code, env)
	}

	// ② 创建首个管理员（Set-Cookie 由 browser 保存）
	admin := newBrowser(srv)
	code, env = apiCall(t, admin, srv, "POST", "/api/v1/auth/setup", map[string]any{
		"username": "admin", "display_name": "管理员", "password": "admin12345",
	}, "")
	if code != 201 {
		t.Fatalf("setup 失败: %d %#v", code, env)
	}
	csrf := dataMap(t, env)["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("setup 未返回 csrf_token")
	}

	// 重复 setup 应被拒绝
	code, _ = apiCall(t, admin, srv, "POST", "/api/v1/auth/setup", map[string]any{
		"username": "x", "password": "xxxxxxxx",
	}, "")
	if code != 409 {
		t.Fatalf("重复 setup 应 409, got %d", code)
	}

	// ③ 当前身份
	code, env = apiCall(t, admin, srv, "GET", "/api/v1/auth/me", nil, csrf)
	if code != 200 || dataMap(t, env)["user"].(map[string]any)["role"] != "admin" {
		t.Fatalf("me 异常: %d %#v", code, env)
	}

	// ④ 管理员能看到 users 模块，且 required_role=admin
	code, env = apiCall(t, admin, srv, "GET", "/api/v1/bootstrap", nil, csrf)
	if code != 200 {
		t.Fatalf("bootstrap 失败: %d %#v", code, env)
	}
	d := dataMap(t, env)
	if d["current_user"].(map[string]any)["username"] != "admin" {
		t.Fatal("bootstrap 应返回当前用户")
	}
	seenUsersModule := false
	for _, raw := range d["modules"].([]any) {
		m := raw.(map[string]any)
		if m["key"] == "users" {
			seenUsersModule = true
			if m["required_role"] != "admin" || m["adapter"] != "users" {
				t.Fatalf("users 模块元数据异常: %#v", m)
			}
		}
	}
	if !seenUsersModule {
		t.Fatal("管理员导航里应出现 users 模块")
	}

	// ⑤ 写请求缺 CSRF → 403
	code, _ = apiCall(t, admin, srv, "POST", "/api/v1/items", map[string]any{
		"module_key": "todo", "title": "缺少 CSRF",
	}, "")
	if code != 403 {
		t.Fatalf("缺 CSRF 应 403, got %d", code)
	}

	// ⑥ 创建 member / viewer 账号
	code, env = apiCall(t, admin, srv, "POST", "/api/v1/users", map[string]any{
		"username": "mama", "display_name": "妈妈", "password": "mama12345",
		"role": "member", "member_id": 2,
	}, csrf)
	if code != 201 {
		t.Fatalf("创建 member 失败: %d %#v", code, env)
	}
	memberID := dataMap(t, env)["id"].(float64)
	code, env = apiCall(t, admin, srv, "POST", "/api/v1/users", map[string]any{
		"username": "guest", "password": "guest12345", "role": "viewer",
	}, csrf)
	if code != 201 {
		t.Fatalf("创建 viewer 失败: %d %#v", code, env)
	}
	// 用户名重复 / 弱密码
	code, _ = apiCall(t, admin, srv, "POST", "/api/v1/users", map[string]any{
		"username": "mama", "password": "another123",
	}, csrf)
	if code != 409 {
		t.Fatalf("重复用户名应 409, got %d", code)
	}
	code, _ = apiCall(t, admin, srv, "POST", "/api/v1/users", map[string]any{
		"username": "weak", "password": "123",
	}, csrf)
	if code != 400 {
		t.Fatalf("弱密码应 400, got %d", code)
	}

	// ⑦ member 登录：看不到 users 模块、不能建模块、但能写条目
	member := newBrowser(srv)
	code, env = apiCall(t, member, srv, "POST", "/api/v1/auth/login", map[string]any{
		"username": "mama", "password": "mama12345",
	}, "")
	if code != 200 {
		t.Fatalf("member 登录失败: %d %#v", code, env)
	}
	mcsrf := dataMap(t, env)["csrf_token"].(string)
	_, env = apiCall(t, member, srv, "GET", "/api/v1/bootstrap", nil, mcsrf)
	for _, raw := range dataMap(t, env)["modules"].([]any) {
		if raw.(map[string]any)["key"] == "users" {
			t.Fatal("member 不应看到 users 模块")
		}
	}
	code, _ = apiCall(t, member, srv, "POST", "/api/v1/modules", map[string]any{
		"key": "x", "name": "x", "view_type": "list",
	}, mcsrf)
	if code != 403 {
		t.Fatalf("member 建模块应 403, got %d", code)
	}
	code, env = apiCall(t, member, srv, "POST", "/api/v1/items", map[string]any{
		"module_key": "todo", "title": "妈妈的任务", "payload": map[string]any{"priority": "P1"},
	}, mcsrf)
	if code != 201 {
		t.Fatalf("member 写条目应 201, got %d %#v", code, env)
	}
	if dataMap(t, env)["created_by"] != memberID {
		t.Fatalf("created_by 审计字段应为 %v, got %#v", memberID, dataMap(t, env)["created_by"])
	}

	// ⑧ viewer 只读
	viewer := newBrowser(srv)
	code, env = apiCall(t, viewer, srv, "POST", "/api/v1/auth/login", map[string]any{
		"username": "guest", "password": "guest12345",
	}, "")
	if code != 200 {
		t.Fatalf("viewer 登录失败: %d %#v", code, env)
	}
	vcsrf := dataMap(t, env)["csrf_token"].(string)
	code, _ = apiCall(t, viewer, srv, "GET", "/api/v1/items", nil, vcsrf)
	if code != 200 {
		t.Fatalf("viewer 应可读, got %d", code)
	}
	code, _ = apiCall(t, viewer, srv, "POST", "/api/v1/items", map[string]any{
		"module_key": "todo", "title": "viewer 不该写",
	}, vcsrf)
	if code != 403 {
		t.Fatalf("viewer 写入应 403, got %d", code)
	}

	// ⑨ 连续错误密码触发锁定
	locked := newBrowser(srv)
	for i := 0; i < maxFailedAttempts; i++ {
		code, _ = apiCall(t, locked, srv, "POST", "/api/v1/auth/login", map[string]any{
			"username": "guest", "password": "wrong-password",
		}, "")
	}
	if code != 429 {
		t.Fatalf("连续失败应 429 ACCOUNT_LOCKED, got %d", code)
	}

	// ⑩ 改密后旧密码失效
	code, env = apiCall(t, member, srv, "POST", "/api/v1/auth/change-password", map[string]any{
		"old_password": "mama12345", "new_password": "newpass123",
	}, mcsrf)
	if code != 200 {
		t.Fatalf("改密失败: %d %#v", code, env)
	}
	code, _ = apiCall(t, newBrowser(srv), srv, "POST", "/api/v1/auth/login", map[string]any{
		"username": "mama", "password": "mama12345",
	}, "")
	if code != 401 {
		t.Fatalf("旧密码应失效, got %d", code)
	}

	// ⑪ 停用账号立即吊销其会话
	code, _ = apiCall(t, admin, srv, "PATCH", "/api/v1/users/3", map[string]any{"enabled": false}, csrf)
	if code != 200 {
		t.Fatalf("停用用户失败: %d", code)
	}
	code, _ = apiCall(t, viewer, srv, "GET", "/api/v1/auth/me", nil, vcsrf)
	if code != 401 {
		t.Fatalf("停用后会话应立即失效, got %d", code)
	}

	// ⑫ 登出后会话失效
	code, _ = apiCall(t, member, srv, "POST", "/api/v1/auth/logout", nil, mcsrf)
	if code != 200 {
		t.Fatalf("登出失败: %d", code)
	}
	code, _ = apiCall(t, member, srv, "GET", "/api/v1/auth/me", nil, mcsrf)
	if code != 401 {
		t.Fatalf("登出后应 401, got %d", code)
	}
}

// API Token（脚本/备份 cron）以 admin 身份访问，且跳过 CSRF。
func TestAPIToken(t *testing.T) {
	srv, _ := newTestAPI(t, "cron-secret")
	h := map[string]string{"Authorization": "Bearer cron-secret"}
	code, _ := call(t, srv, "GET", "/api/v1/users", nil, h)
	if code != 200 {
		t.Fatalf("API Token 应可访问 admin 端点, got %d", code)
	}
	// 无 CSRF 也能写（脚本不需要浏览器 CSRF 头）
	code, _ = call(t, srv, "POST", "/api/v1/admin/backup", nil, h)
	if code != 200 {
		t.Fatalf("API Token 写操作应跳过 CSRF, got %d", code)
	}
	code, _ = call(t, srv, "GET", "/api/v1/users", nil, nil)
	if code != 401 {
		t.Fatalf("无凭证应 401, got %d", code)
	}
}
