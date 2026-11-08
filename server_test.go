package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newTestAPI(t *testing.T, password string) (*httptest.Server, *Store) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "family.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// 与 main 保持一致：配置口令时自动创建首个管理员
	if _, err := st.EnsureBootstrapAdmin(context.Background(), password); err != nil {
		t.Fatalf("EnsureBootstrapAdmin: %v", err)
	}
	html, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("读取内嵌前端: %v", err)
	}
	srv := httptest.NewServer((&API{store: st, password: password, indexHTML: html}).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any, headers map[string]string) (int, map[string]any) {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应 %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func dataMap(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	d, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data: %#v", env)
	}
	return d
}

// 首屏：一次拿到注册表 + 成员；静态页同端口可访问。
func TestBootstrapAndStatic(t *testing.T) {
	srv, _ := newTestAPI(t, "")
	code, env := call(t, srv, "GET", "/api/v1/bootstrap", nil, nil)
	if code != 200 || env["ok"] != true {
		t.Fatalf("bootstrap 失败: %d %#v", code, env)
	}
	d := dataMap(t, env)
	if got := len(d["modules"].([]any)); got != 4 {
		t.Fatalf("内置模块应为 4 个(含 users), got %d", got)
	}
	if got := len(d["members"].([]any)); got != 4 {
		t.Fatalf("成员应为 4 个, got %d", got)
	}
	modules := d["modules"].([]any)
	first := modules[0].(map[string]any)
	if got := len(first["schema"].([]any)); got != 5 {
		t.Fatalf("calendar schema 应含 3 core + 2 自定义 = 5, got %d", got)
	}
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("静态页异常: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// 核心验收：新增模块不写 DDL、不改查询，注册表驱动的表单与导航直接生效。
func TestExtendModuleWithoutDDL(t *testing.T) {
	srv, _ := newTestAPI(t, "")
	code, env := call(t, srv, "POST", "/api/v1/modules", map[string]any{
		"key": "budget", "name": "家庭账本", "icon": "chart", "view_type": "list",
		"desc": "记录家庭开支",
		"schema": []any{
			map[string]any{"key": "amount", "label": "金额", "type": "number"},
			map[string]any{"key": "category", "label": "分类", "type": "select", "options": []string{"餐饮", "日用", "医疗"}},
		},
	}, nil)
	if code != 201 {
		t.Fatalf("新建模块失败: %d %#v", code, env)
	}
	m := dataMap(t, env)
	if got := len(m["schema"].([]any)); got != 5 {
		t.Fatalf("schema 应为 3 core + 2 自定义 = 5, got %d", got)
	}

	// 用新模块写数据，无需任何后端改动
	code, env = call(t, srv, "POST", "/api/v1/items", map[string]any{
		"module_key": "budget", "title": "本月买菜", "occur_at": "2026-01-15",
		"member_id": 2, "payload": map[string]any{"amount": 860, "category": "餐饮"},
	}, nil)
	if code != 201 {
		t.Fatalf("写入自定义模块条目失败: %d %#v", code, env)
	}
	it := dataMap(t, env)
	payload := it["payload"].(map[string]any)
	if payload["amount"] != float64(860) || payload["category"] != "餐饮" {
		t.Fatalf("payload 落库异常: %#v", payload)
	}
	itemID := it["id"].(string)

	code, env = call(t, srv, "GET", "/api/v1/items?module=budget", nil, nil)
	if code != 200 || dataMap(t, env)["total"] != float64(1) {
		t.Fatalf("按模块过滤异常: %d %#v", code, env)
	}
	_, env = call(t, srv, "GET", "/api/v1/bootstrap", nil, nil)
	if got := len(dataMap(t, env)["modules"].([]any)); got != 5 {
		t.Fatalf("新模块应自动进入注册表(导航), got %d", got)
	}

	// 重复 key 返回 409
	code, env = call(t, srv, "POST", "/api/v1/modules", map[string]any{
		"key": "budget", "name": "重复", "view_type": "list",
	}, nil)
	if code != 409 {
		t.Fatalf("重复 key 应 409, got %d %#v", code, env)
	}
	_ = itemID
}

// payload 严格按 schema 校验：未知字段 / 非法枚举 / 类型不符 / 模块不存在。
func TestPayloadValidation(t *testing.T) {
	srv, _ := newTestAPI(t, "")
	cases := []struct {
		name string
		body map[string]any
	}{
		{"未声明字段", map[string]any{"module_key": "todo", "title": "x", "payload": map[string]any{"hack": "v"}}},
		{"枚举非法", map[string]any{"module_key": "todo", "title": "x", "payload": map[string]any{"priority": "P9"}}},
		{"类型不符", map[string]any{"module_key": "health", "title": "x", "payload": map[string]any{"kind": "就医", "detail": 123}}},
		{"core 字段误入 payload", map[string]any{"module_key": "todo", "title": "x", "payload": map[string]any{"title": "y"}}},
		{"模块不存在", map[string]any{"module_key": "ghost", "title": "x"}},
		{"日期格式错误", map[string]any{"module_key": "todo", "title": "x", "occur_at": "2026/01/15"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, env := call(t, srv, "POST", "/api/v1/items", c.body, nil)
			if code != 400 {
				t.Fatalf("应返回 400, got %d %#v", code, env)
			}
			if dataMapOrNil(env["error"])["code"] != "INVALID_ARGUMENT" {
				t.Fatalf("错误码异常: %#v", env)
			}
		})
	}
}

func dataMapOrNil(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// 条目 CRUD、状态筛选、软删除。
func TestItemCRUDAndSoftDelete(t *testing.T) {
	srv, _ := newTestAPI(t, "")
	code, env := call(t, srv, "POST", "/api/v1/items", map[string]any{
		"module_key": "todo", "title": "测试待办", "occur_at": "2026-02-01",
		"payload": map[string]any{"priority": "P1"},
	}, nil)
	if code != 201 {
		t.Fatalf("创建失败: %d %#v", code, env)
	}
	id := dataMap(t, env)["id"].(string)

	code, env = call(t, srv, "PATCH", "/api/v1/items/"+id, map[string]any{
		"status": "done", "payload": map[string]any{"note": "已完成"},
	}, nil)
	if code != 200 || dataMap(t, env)["status"] != "done" {
		t.Fatalf("更新失败: %d %#v", code, env)
	}
	if dataMap(t, env)["payload"].(map[string]any)["priority"] != "P1" {
		t.Fatal("payload 合并丢失了原字段")
	}

	_, env = call(t, srv, "GET", "/api/v1/items?module=todo&status=open", nil, nil)
	for _, raw := range dataMap(t, env)["items"].([]any) {
		if raw.(map[string]any)["id"] == id {
			t.Fatal("已完成的条目仍出现在 open 列表")
		}
	}

	code, _ = call(t, srv, "DELETE", "/api/v1/items/"+id, nil, nil)
	if code != 200 {
		t.Fatalf("删除失败: %d", code)
	}
	code, env = call(t, srv, "GET", "/api/v1/items/"+id, nil, nil)
	if code != 404 {
		t.Fatalf("软删后应 404, got %d %#v", code, env)
	}
	code, _ = call(t, srv, "DELETE", "/api/v1/items/"+id, nil, nil)
	if code != 404 {
		t.Fatalf("重复删除应 404, got %d", code)
	}

	// 撤销删除：软删只是标记，恢复后 id / 标题 / payload 原样返回。
	code, env = call(t, srv, "POST", "/api/v1/items/"+id+"/restore", nil, nil)
	if code != 200 || dataMap(t, env)["id"] != id {
		t.Fatalf("恢复失败: %d %#v", code, env)
	}
	code, env = call(t, srv, "GET", "/api/v1/items/"+id, nil, nil)
	if code != 200 || dataMap(t, env)["title"] != "测试待办" {
		t.Fatalf("恢复后应可查询: %d %#v", code, env)
	}
	if dataMap(t, env)["payload"].(map[string]any)["priority"] != "P1" {
		t.Fatal("恢复后 payload 丢失")
	}
	code, _ = call(t, srv, "POST", "/api/v1/items/"+id+"/restore", nil, nil)
	if code != 404 {
		t.Fatalf("重复恢复应 404, got %d", code)
	}
}

// 首页一次聚合跨模块数据，且能识别「未来就医」。
func TestOverviewAggregation(t *testing.T) {
	srv, _ := newTestAPI(t, "")
	code, env := call(t, srv, "GET", "/api/v1/overview", nil, nil)
	if code != 200 {
		t.Fatalf("overview 失败: %d %#v", code, env)
	}
	d := dataMap(t, env)
	stats := d["stats"].(map[string]any)
	if stats["today"].(float64) < 2 {
		t.Fatalf("今日应包含日历/待办/健康多个模块, got %#v", stats)
	}
	if stats["upcoming_visits"].(float64) < 1 {
		t.Fatalf("未识别未来就医安排: %#v", stats)
	}
	if stats["open_todo"].(float64) < 1 {
		t.Fatalf("未聚合未完成待办: %#v", stats)
	}
	if len(d["today_items"].([]any)) == 0 || len(d["open_todos"].([]any)) == 0 {
		t.Fatalf("聚合列表为空: %#v", d)
	}
	// 非法参数
	code, _ = call(t, srv, "GET", "/api/v1/overview?date=bad", nil, nil)
	if code != 400 {
		t.Fatalf("非法日期应 400, got %d", code)
	}
}

// 在线备份产物是完整可读的 SQLite 库。
func TestBackup(t *testing.T) {
	srv, _ := newTestAPI(t, "")
	code, env := call(t, srv, "POST", "/api/v1/admin/backup", nil, nil)
	if code != 200 {
		t.Fatalf("备份失败: %d %#v", code, env)
	}
	path := dataMap(t, env)["path"].(string)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("备份库不可读: %v", err)
	}
	if n < 5 {
		t.Fatalf("备份数据不完整, items=%d", n)
	}
}

// 配置口令后 API 需鉴权，健康检查放行。
func TestAuth(t *testing.T) {
	srv, _ := newTestAPI(t, "secret123")
	code, _ := call(t, srv, "GET", "/api/v1/modules", nil, nil)
	if code != 401 {
		t.Fatalf("无口令应 401, got %d", code)
	}
	code, _ = call(t, srv, "GET", "/api/v1/modules", nil, map[string]string{"Authorization": "Bearer wrong"})
	if code != 401 {
		t.Fatalf("错误口令应 401, got %d", code)
	}
	code, _ = call(t, srv, "GET", "/api/v1/modules", nil, map[string]string{"Authorization": "Bearer secret123"})
	if code != 200 {
		t.Fatalf("正确口令应 200, got %d", code)
	}
	code, _ = call(t, srv, "GET", "/api/v1/health", nil, nil)
	if code != 200 {
		t.Fatalf("健康检查应放行, got %d", code)
	}
}
