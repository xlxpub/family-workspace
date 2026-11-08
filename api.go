package main

// HTTP 层：统一响应信封、路由、参数校验、payload 按 schema 校验、角色权限。
// 前端只依赖 /api/v1 下的端点与 JSON 形状，不感知后端存储细节。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// envelope 是前后端唯一的响应契约：成功 {ok:true,data:...}，失败 {ok:false,error:{code,message}}。
type envelope struct {
	OK    bool      `json:"ok"`
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

func ok(w http.ResponseWriter, data any) { writeJSON(w, http.StatusOK, envelope{OK: true, Data: data}) }

func created(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusCreated, envelope{OK: true, Data: data})
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, envelope{OK: false, Error: &apiError{Code: code, Message: msg}})
}

func failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, ErrConflict):
		fail(w, http.StatusConflict, "CONFLICT", err.Error())
	default:
		log.Printf("internal error: %v", err)
		fail(w, http.StatusInternalServerError, "INTERNAL", "服务内部错误")
	}
}

func badRequest(w http.ResponseWriter, msg string) {
	fail(w, http.StatusBadRequest, "INVALID_ARGUMENT", msg)
}

type API struct {
	store        *Store
	password     string // FW_AUTH_PASSWORD：初始管理员密码 + 脚本 API Token
	cookieSecure bool   // FW_COOKIE_SECURE=1（HTTPS 反代时开启）
	indexHTML    []byte
	ai           *AIClient
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", a.health)
	mux.HandleFunc("/api/v1/bootstrap", a.bootstrap) // 首屏一次拿 modules + members + 当前用户
	mux.HandleFunc("/api/v1/modules", a.modules)
	mux.HandleFunc("/api/v1/modules/", a.moduleDetail)
	mux.HandleFunc("/api/v1/members", a.members)
	mux.HandleFunc("/api/v1/items", a.items)
	mux.HandleFunc("/api/v1/items/parse", a.parseItems)
	mux.HandleFunc("/api/v1/items/", a.itemDetail)
	mux.HandleFunc("/api/v1/overview", a.overview)
	mux.HandleFunc("/api/v1/admin/backup", a.backup)
	// 用户与鉴权模块
	mux.HandleFunc("/api/v1/auth/login", a.authLogin)
	mux.HandleFunc("/api/v1/auth/logout", a.authLogout)
	mux.HandleFunc("/api/v1/auth/status", a.authStatus)
	mux.HandleFunc("/api/v1/auth/setup", a.authSetup)
	mux.HandleFunc("/api/v1/auth/me", a.authMe)
	mux.HandleFunc("/api/v1/auth/change-password", a.authChangePassword)
	mux.HandleFunc("/api/v1/users", a.users)
	mux.HandleFunc("/api/v1/users/", a.userDetail)
	mux.HandleFunc("/", a.static)
	return a.recoverMW(a.loggingMW(a.authMW(mux)))
}

// ---------------- 中间件 ----------------

func (a *API) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, v)
				fail(w, http.StatusInternalServerError, "INTERNAL", "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) loggingMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// ---------------- 通用端点 ----------------

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ok(w, map[string]any{"status": "ok", "time": nowStr(), "db": "sqlite"})
}

func (a *API) bootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	mods, err := a.store.ListModules(r.Context(), currentUser(r).Role)
	if err != nil {
		failErr(w, err)
		return
	}
	members, err := a.store.ListMembers(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]any{
		"modules": mods, "members": members, "server_time": nowStr(),
		"current_user": currentUser(r), "csrf_token": csrfFrom(r),
		"ai_enabled": a.ai != nil && currentUser(r).Username != "__setup__",
	})
}

func (a *API) members(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	list, err := a.store.ListMembers(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]any{"items": list})
}

func (a *API) static(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		fail(w, http.StatusNotFound, "NOT_FOUND", "路径不存在")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := w.Write(a.indexHTML); err != nil {
		log.Printf("写静态页失败: %v", err)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	fail(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "不支持的请求方法")
}

// ---------------- modules ----------------

var (
	moduleKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	fieldKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	validViews  = map[string]bool{"list": true, "calendar": true, "timeline": true, "board": true, "chart": true, "users": true}
	validTypes  = map[string]bool{"text": true, "textarea": true, "date": true, "number": true, "select": true, "member": true}
	validStatus = map[string]bool{"open": true, "done": true, "cancelled": true}
)

type createModuleReq struct {
	Key          string  `json:"key"`
	Name         string  `json:"name"`
	Icon         string  `json:"icon"`
	ViewType     string  `json:"view_type"`
	Desc         string  `json:"desc"`
	RequiredRole string  `json:"required_role"`
	Schema       []Field `json:"schema"` // 只需传自定义字段，core 字段由后端补齐
}

type patchModuleReq struct {
	Enabled   *bool   `json:"enabled"`
	SortOrder *int    `json:"sort_order"`
	Name      *string `json:"name"`
}

func (a *API) modules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.store.ListModules(r.Context(), currentUser(r).Role)
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, map[string]any{"items": list, "total": len(list)})
	case http.MethodPost:
		if !a.requireRole(w, r, "admin") {
			return
		}
		a.createModule(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) createModule(w http.ResponseWriter, r *http.Request) {
	var req createModuleReq
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err.Error())
		return
	}
	if !moduleKeyRe.MatchString(req.Key) {
		badRequest(w, "key 需匹配 ^[a-z][a-z0-9_-]{1,31}$")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		badRequest(w, "name 不能为空")
		return
	}
	if !validViews[req.ViewType] {
		badRequest(w, "view_type 必须是 list/calendar/timeline/board/chart/users 之一")
		return
	}
	if req.RequiredRole == "" {
		req.RequiredRole = "viewer"
	}
	if !validRole(req.RequiredRole) {
		badRequest(w, "required_role 必须是 viewer/member/admin")
		return
	}
	if req.Icon == "" {
		req.Icon = "grid"
	}
	core := map[string]bool{"title": true, "occur_at": true, "member_id": true}
	for i := range req.Schema {
		f := &req.Schema[i]
		if !fieldKeyRe.MatchString(f.Key) {
			badRequest(w, fmt.Sprintf("字段 key %q 非法", f.Key))
			return
		}
		if core[f.Key] {
			badRequest(w, fmt.Sprintf("字段 %q 是内置通用列，无需声明", f.Key))
			return
		}
		if !validTypes[f.Type] {
			badRequest(w, fmt.Sprintf("字段 %q 的 type %q 不支持", f.Key, f.Type))
			return
		}
		if f.Type == "select" && len(f.Options) == 0 {
			badRequest(w, fmt.Sprintf("select 字段 %q 必须提供 options", f.Key))
			return
		}
		f.Core = false
	}
	existing, err := a.store.ListModules(r.Context(), "admin")
	if err != nil {
		failErr(w, err)
		return
	}
	m := Module{
		Key: req.Key, Name: strings.TrimSpace(req.Name), Icon: req.Icon,
		ViewType: req.ViewType, Desc: req.Desc,
		Schema:  append(coreSchema(), req.Schema...),
		Adapter: "generic", RequiredRole: req.RequiredRole,
		SortOrder: 100 + len(existing), Enabled: true, Builtin: false,
	}
	if err := a.store.CreateModule(r.Context(), &m); err != nil {
		failErr(w, err)
		return
	}
	created(w, m)
}

func (a *API) moduleDetail(w http.ResponseWriter, r *http.Request) {
	if !a.requireRole(w, r, "admin") {
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/api/v1/modules/")
	if key == "" || strings.Contains(key, "/") {
		badRequest(w, "模块 key 非法")
		return
	}
	if r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var req patchModuleReq
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err.Error())
		return
	}
	m, err := a.store.UpdateModule(r.Context(), key, req.Enabled, req.SortOrder, req.Name)
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, m)
}

// ---------------- items ----------------

type createItemReq struct {
	ModuleKey string         `json:"module_key"`
	Title     string         `json:"title"`
	OccurAt   *string        `json:"occur_at"`
	EndAt     *string        `json:"end_at"`
	MemberID  *int64         `json:"member_id"`
	Status    string         `json:"status"`
	Payload   map[string]any `json:"payload"`
}

func (a *API) items(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listItems(w, r)
	case http.MethodPost:
		if !a.requireRole(w, r, "member") {
			return
		}
		a.createItem(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) listItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ItemFilter{
		From: q.Get("from"), To: q.Get("to"), Module: q.Get("module"),
		Status: q.Get("status"), Q: q.Get("q"), Limit: 100,
	}
	if f.From != "" && parseDate(f.From) != nil {
		badRequest(w, "from 需为 YYYY-MM-DD")
		return
	}
	if f.To != "" && parseDate(f.To) != nil {
		badRequest(w, "to 需为 YYYY-MM-DD")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badRequest(w, "limit 非法")
			return
		}
		if n > 200 {
			n = 200
		}
		if n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badRequest(w, "offset 非法")
			return
		}
		f.Offset = n
	}
	var err error
	f.MemberID, err = parseMember(q.Get("member"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	list, total, err := a.store.ListItems(r.Context(), f)
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]any{"items": list, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (a *API) createItem(w http.ResponseWriter, r *http.Request) {
	var req createItemReq
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err.Error())
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		badRequest(w, "title 不能为空")
		return
	}
	m, err := a.store.GetModule(r.Context(), req.ModuleKey)
	if errors.Is(err, ErrNotFound) {
		badRequest(w, "module_key 不存在，请先注册模块")
		return
	}
	if err != nil {
		failErr(w, err)
		return
	}
	if !m.Enabled {
		badRequest(w, "该模块已停用")
		return
	}
	if !a.requireRole(w, r, m.RequiredRole) {
		return
	}
	if req.MemberID != nil {
		members, err := a.store.ListMembers(r.Context())
		if err != nil {
			failErr(w, err)
			return
		}
		exists := false
		for _, member := range members {
			if member.ID == *req.MemberID {
				exists = true
				break
			}
		}
		if !exists {
			badRequest(w, "member_id 不存在")
			return
		}
	}
	// adapter != generic 的模块（如 users）走独立表，不允许写进 items。
	if m.Adapter != "generic" {
		badRequest(w, "该模块使用独立存储(adapter="+m.Adapter+")，请走专用接口")
		return
	}
	if req.OccurAt != nil && parseDate(*req.OccurAt) != nil {
		badRequest(w, "occur_at 需为 YYYY-MM-DD")
		return
	}
	if req.EndAt != nil && parseDate(*req.EndAt) != nil {
		badRequest(w, "end_at 需为 YYYY-MM-DD")
		return
	}
	if req.Status == "" {
		req.Status = "open"
	}
	if !validStatus[req.Status] {
		badRequest(w, "status 必须是 open/done/cancelled 之一")
		return
	}
	payload := req.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if err := validatePayload(m, payload); err != nil {
		badRequest(w, err.Error())
		return
	}
	it := &Item{
		ModuleKey: req.ModuleKey, Title: strings.TrimSpace(req.Title),
		OccurAt: req.OccurAt, EndAt: req.EndAt, MemberID: req.MemberID,
		Status: req.Status, Payload: payload,
		CreatedBy: userIDPtr(currentUser(r)),
	}
	if err := a.store.CreateItem(r.Context(), it); err != nil {
		failErr(w, err)
		return
	}
	created(w, it)
}

func (a *API) itemDetail(w http.ResponseWriter, r *http.Request) {
	id, action := strings.TrimPrefix(r.URL.Path, "/api/v1/items/"), ""
	if i := strings.Index(id, "/"); i >= 0 {
		id, action = id[:i], id[i+1:]
	}
	if id == "" || (action != "" && action != "restore") {
		badRequest(w, "条目 id 非法")
		return
	}
	// 撤销删除：软删只写 deleted_at，恢复时原 id 与全部字段保持不变。
	if action == "restore" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if !a.requireRole(w, r, "member") {
			return
		}
		if err := a.store.RestoreItem(r.Context(), id); err != nil {
			failErr(w, err)
			return
		}
		it, err := a.store.GetItem(r.Context(), id)
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, it)
		return
	}
	switch r.Method {
	case http.MethodGet:
		it, err := a.store.GetItem(r.Context(), id)
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, it)
	case http.MethodPatch, http.MethodDelete:
		if !a.requireRole(w, r, "member") {
			return
		}
		if r.Method == http.MethodDelete {
			if err := a.store.SoftDeleteItem(r.Context(), id); err != nil {
				failErr(w, err)
				return
			}
			ok(w, map[string]any{"id": id, "deleted": true})
			return
		}
		var p ItemPatch
		if err := decodeJSON(r, &p); err != nil {
			badRequest(w, err.Error())
			return
		}
		if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
			badRequest(w, "title 不能为空")
			return
		}
		if p.OccurAt != nil && parseDate(*p.OccurAt) != nil {
			badRequest(w, "occur_at 需为 YYYY-MM-DD")
			return
		}
		if p.Status != nil && !validStatus[*p.Status] {
			badRequest(w, "status 必须是 open/done/cancelled 之一")
			return
		}
		if p.Payload != nil {
			current, err := a.store.GetItem(r.Context(), id)
			if err != nil {
				failErr(w, err)
				return
			}
			m, err := a.store.GetModule(r.Context(), current.ModuleKey)
			if err != nil {
				failErr(w, err)
				return
			}
			if err := validatePayload(m, *p.Payload); err != nil {
				badRequest(w, err.Error())
				return
			}
		}
		it, err := a.store.UpdateItem(r.Context(), id, p)
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, it)
	default:
		methodNotAllowed(w)
	}
}

// ---------------- 首页聚合 / 备份 ----------------

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	q := r.URL.Query()
	date := q.Get("date")
	if date != "" && parseDate(date) != nil {
		badRequest(w, "date 需为 YYYY-MM-DD")
		return
	}
	days := 7
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 31 {
			badRequest(w, "days 需为 1-31")
			return
		}
		days = n
	}
	ov, err := a.store.Overview(r.Context(), date, days)
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, ov)
}

func (a *API) backup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !a.requireRole(w, r, "admin") {
		return
	}
	dest, err := a.store.Backup(r.Context(), "")
	if err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]any{"path": dest})
}

// ---------------- 校验工具 ----------------

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("请求体只能包含一个 JSON 对象")
	}
	return nil
}

// validatePayload 保证 payload 里只有 schema 声明过的字段，且值类型/枚举合法。
func validatePayload(m Module, p map[string]any) error {
	allowed := map[string]Field{}
	for _, f := range m.Schema {
		allowed[f.Key] = f
	}
	for k, v := range p {
		f, exists := allowed[k]
		if !exists {
			return fmt.Errorf("字段 %q 未在模块 schema 中声明", k)
		}
		if f.Core {
			return fmt.Errorf("字段 %q 属于通用列，必须放在请求顶层", k)
		}
		if err := validateValue(f, v); err != nil {
			return fmt.Errorf("字段 %q: %w", k, err)
		}
	}
	return nil
}

func validateValue(f Field, v any) error {
	if v == nil {
		return nil
	}
	switch f.Type {
	case "select":
		s, isStr := v.(string)
		if !isStr {
			return errors.New("应为字符串")
		}
		for _, opt := range f.Options {
			if opt == s {
				return nil
			}
		}
		return fmt.Errorf("值 %q 不在可选项 %v 中", s, f.Options)
	case "number":
		switch v.(type) {
		case float64, float32, int, int64, json.Number:
			return nil
		}
		return errors.New("应为数字")
	case "date":
		s, isStr := v.(string)
		if !isStr {
			return errors.New("应为字符串")
		}
		return parseDate(s)
	default:
		if _, isStr := v.(string); !isStr {
			return errors.New("应为字符串")
		}
	}
	return nil
}
