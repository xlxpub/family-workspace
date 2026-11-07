package main

// 认证与用户模块 HTTP 层。
// 浏览器：Session Cookie(HttpOnly/SameSite=Lax) + 写操作 X-FW-CSRF。
// 脚本：Authorization: Bearer <FW_AUTH_PASSWORD> 作为 API Token（admin，跳过 CSRF）。

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const (
	userCtxKey ctxKey = iota
	csrfCtxKey
)

func currentUser(r *http.Request) User {
	if u, ok := r.Context().Value(userCtxKey).(User); ok {
		return u
	}
	return User{Role: "viewer"} // 兜底最小权限
}

func csrfFrom(r *http.Request) string {
	if v, ok := r.Context().Value(csrfCtxKey).(string); ok {
		return v
	}
	return ""
}

// 虚拟身份（API Token / 安装模式）没有真实 user id，不写审计字段。
func userIDPtr(u User) *int64 {
	if u.ID <= 0 {
		return nil
	}
	v := u.ID
	return &v
}

func isPublicAPIPath(p string) bool {
	switch p {
	case "/api/v1/health", "/api/v1/auth/login", "/api/v1/auth/logout",
		"/api/v1/auth/status", "/api/v1/auth/setup":
		return true
	}
	return false
}

func isWriteMethod(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch || m == http.MethodDelete
}

// authMW 替换了原来的「全家共享单口令」：现在解析出真实 User 放进 context。
func (a *API) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") || isPublicAPIPath(p) {
			next.ServeHTTP(w, r)
			return
		}
		n, err := a.store.CountUsers(r.Context())
		if err != nil {
			failErr(w, err)
			return
		}
		if n == 0 {
			// 安装模式：等价于临时管理员，便于完成首次初始化
			ctx := context.WithValue(r.Context(), userCtxKey,
				User{Role: "admin", Username: "__setup__", DisplayName: "安装模式"})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// ① API Token（脚本 / 备份 cron）
		tok := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimPrefix(h, "Bearer ")
		}
		if tok == "" {
			tok = r.Header.Get("X-FW-Token")
		}
		if a.password != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.password)) == 1 {
			ctx := context.WithValue(r.Context(), userCtxKey,
				User{Role: "admin", Username: "__api_token__", DisplayName: "API Token"})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// ② Session Cookie
		c, err := r.Cookie(cookieName)
		if err != nil {
			fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
			return
		}
		u, csrf, err := a.store.ResolveSession(r.Context(), c.Value)
		if err != nil {
			fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "会话已失效，请重新登录")
			return
		}
		if isWriteMethod(r.Method) &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-FW-CSRF")), []byte(csrf)) != 1 {
			fail(w, http.StatusForbidden, "FORBIDDEN", "CSRF 校验失败：写请求需携带 X-FW-CSRF")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, u)
		ctx = context.WithValue(ctx, csrfCtxKey, csrf)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) requireRole(w http.ResponseWriter, r *http.Request, need string) bool {
	if !roleAllows(currentUser(r).Role, need) {
		fail(w, http.StatusForbidden, "FORBIDDEN", "权限不足：需要 "+need+" 及以上角色")
		return false
	}
	return true
}

func (a *API) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: raw, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cookieSecure,
		MaxAge: int(sessionTTL.Seconds()), Expires: time.Now().Add(sessionTTL),
	})
}

func (a *API) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cookieSecure,
		MaxAge: -1, Expires: time.Unix(0, 0),
	})
}

// ---------------- 认证端点 ----------------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *API) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req loginReq
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err.Error())
		return
	}
	u, err := a.store.Authenticate(r.Context(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrAccountLocked):
			fail(w, http.StatusTooManyRequests, "ACCOUNT_LOCKED", err.Error())
		case errors.Is(err, ErrAccountDisabled):
			fail(w, http.StatusForbidden, "ACCOUNT_DISABLED", err.Error())
		default:
			fail(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		}
		return
	}
	raw, csrf, err := a.store.CreateSession(r.Context(), u.ID)
	if err != nil {
		failErr(w, err)
		return
	}
	a.setSessionCookie(w, raw)
	ok(w, map[string]any{"user": u, "csrf_token": csrf, "expires_in": int(sessionTTL.Seconds())})
}

func (a *API) authLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if err := a.store.DeleteSession(r.Context(), c.Value); err != nil {
			log.Printf("删除会话失败: %v", err)
		}
	}
	a.clearSessionCookie(w)
	ok(w, map[string]any{"logged_out": true})
}

func (a *API) authStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	n, err := a.store.CountUsers(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	resp := map[string]any{"setup_required": n == 0, "authenticated": false}
	if c, err := r.Cookie(cookieName); err == nil {
		if u, csrf, err := a.store.ResolveSession(r.Context(), c.Value); err == nil {
			resp["authenticated"] = true
			resp["user"] = u
			resp["csrf_token"] = csrf
		}
	}
	ok(w, resp)
}

func (a *API) authSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	n, err := a.store.CountUsers(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	if n > 0 {
		fail(w, http.StatusConflict, "CONFLICT", "已完成初始化，请通过「用户与权限」模块添加账号")
		return
	}
	var req struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err.Error())
		return
	}
	u, err := a.store.CreateUser(r.Context(), req.Username, req.DisplayName, req.Password, "admin", nil)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			failErr(w, err)
		} else {
			badRequest(w, err.Error())
		}
		return
	}
	raw, csrf, err := a.store.CreateSession(r.Context(), u.ID)
	if err != nil {
		failErr(w, err)
		return
	}
	a.setSessionCookie(w, raw)
	created(w, map[string]any{"user": u, "csrf_token": csrf})
}

func (a *API) authMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ok(w, map[string]any{"user": currentUser(r), "csrf_token": csrfFrom(r)})
}

func (a *API) authChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	u := currentUser(r)
	if u.ID <= 0 {
		badRequest(w, "API Token / 安装模式不支持改密，请用账号登录后操作")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err.Error())
		return
	}
	if _, err := a.store.Authenticate(r.Context(), u.Username, req.OldPassword); err != nil {
		fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "原密码不正确")
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		badRequest(w, err.Error())
		return
	}
	if _, err := a.store.UpdateUser(r.Context(), u.ID, UserPatch{Password: &req.NewPassword}); err != nil {
		failErr(w, err)
		return
	}
	ok(w, map[string]any{"changed": true})
}

// ---------------- 用户管理端点（admin） ----------------

type createUserReq struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	Role        string `json:"role"`
	MemberID    *int64 `json:"member_id"`
}

func (a *API) users(w http.ResponseWriter, r *http.Request) {
	if !a.requireRole(w, r, "admin") {
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := a.store.ListUsers(r.Context())
		if err != nil {
			failErr(w, err)
			return
		}
		ok(w, map[string]any{"items": list, "total": len(list)})
	case http.MethodPost:
		var req createUserReq
		if err := decodeJSON(r, &req); err != nil {
			badRequest(w, err.Error())
			return
		}
		if req.Role == "" {
			req.Role = "member"
		}
		u, err := a.store.CreateUser(r.Context(), req.Username, req.DisplayName,
			req.Password, req.Role, req.MemberID)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				failErr(w, err)
			} else {
				badRequest(w, err.Error())
			}
			return
		}
		created(w, u)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) userDetail(w http.ResponseWriter, r *http.Request) {
	if !a.requireRole(w, r, "admin") {
		return
	}
	rawID := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if rawID == "" || strings.Contains(rawID, "/") || err != nil || id <= 0 {
		badRequest(w, "用户 id 非法")
		return
	}
	if r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var p UserPatch
	if err := decodeJSON(r, &p); err != nil {
		badRequest(w, err.Error())
		return
	}
	me := currentUser(r)
	if id == me.ID {
		if p.Enabled != nil && !*p.Enabled {
			badRequest(w, "不能停用自己的账号")
			return
		}
		if p.Role != nil && *p.Role != "admin" {
			badRequest(w, "不能降低自己的角色，避免锁死最后一个管理员")
			return
		}
	}
	u, err := a.store.UpdateUser(r.Context(), id, p)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			failErr(w, err)
		} else {
			badRequest(w, err.Error())
		}
		return
	}
	ok(w, u)
}
