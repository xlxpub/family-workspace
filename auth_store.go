package main

// 用户与会话存储层。
// 关键区分：members 是「家庭成员」业务维度（条目归属/日历分色，小孩外婆可无账号）；
// users 是「登录账号」身份维度。二者通过 member_id 可选绑定，不合并成一张表。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionTTL        = 7 * 24 * time.Hour
	maxFailedAttempts = 5
	lockDuration      = 15 * time.Minute
	cookieName        = "fw_session"
)

var (
	ErrBadCredentials  = errors.New("用户名或密码错误")
	ErrAccountLocked   = errors.New("登录失败次数过多，账号已临时锁定，请 15 分钟后再试")
	ErrAccountDisabled = errors.New("账号已停用")
)

// User 是对外形状，绝不包含 password_hash / failed_attempts / locked_until。
type User struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"` // admin|member|viewer
	MemberID    *int64  `json:"member_id"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   string  `json:"created_at"`
	LastLoginAt *string `json:"last_login_at"`
}

type UserPatch struct {
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
	Enabled     *bool   `json:"enabled"`
	MemberID    *int64  `json:"member_id"` // 0 = 解绑成员
	Password    *string `json:"password"`
}

const userColumns = `id,username,display_name,role,member_id,enabled,created_at,last_login_at`

type userScanner interface{ Scan(dest ...any) error }

func scanUser(sc userScanner) (User, error) {
	var u User
	var memberID sql.NullInt64
	var enabled int
	var lastLogin sql.NullString
	err := sc.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &memberID,
		&enabled, &u.CreatedAt, &lastLogin)
	if err != nil {
		return u, err
	}
	if memberID.Valid {
		v := memberID.Int64
		u.MemberID = &v
	}
	if lastLogin.Valid {
		v := lastLogin.String
		u.LastLoginAt = &v
	}
	u.Enabled = enabled != 0
	return u, nil
}

func newSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d%d", time.Now().UnixNano(), n)
	}
	return hex.EncodeToString(b)
}

// DB 里只存会话 ID 的哈希，库文件泄露也无法直接冒用会话。
func sessionHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func validRole(r string) bool { return r == "admin" || r == "member" || r == "viewer" }

func validatePassword(p string) error {
	// bcrypt 硬限制 72 字节；过短密码在家庭内网也容易被猜到。
	if len(p) < 8 || len(p) > 72 {
		return errors.New("密码长度需为 8-72 字节")
	}
	return nil
}

func roleAllows(has, need string) bool {
	rank := map[string]int{"viewer": 1, "member": 2, "admin": 3}
	return rank[has] >= rank[need]
}

// rolesUpTo 用于模块注册表的 required_role 过滤。
func rolesUpTo(role string) []string {
	switch role {
	case "admin":
		return []string{"viewer", "member", "admin"}
	case "member":
		return []string{"viewer", "member"}
	default:
		return []string{"viewer"}
	}
}

// ---------------- users ----------------

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计用户: %w", err)
	}
	return n, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询用户: %w", err)
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描用户: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户: %w", err)
	}
	return out, nil
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, fmt.Errorf("查询用户(%d): %w", id, err)
	}
	return u, nil
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, password, role string, memberID *int64) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 32 {
		return User{}, fmt.Errorf("用户名需为 1-32 字符")
	}
	if displayName == "" {
		displayName = username
	}
	if err := validatePassword(password); err != nil {
		return User{}, err
	}
	if !validRole(role) {
		return User{}, fmt.Errorf("role 必须是 admin/member/viewer")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("生成密码哈希: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO users
		(username,display_name,password_hash,role,member_id,enabled,created_at)
		VALUES(?,?,?,?,?,1,?)`,
		username, displayName, string(hash), role, nullInt(memberID), nowStr())
	if err != nil {
		if isUniqueErr(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("写入用户: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("读取用户 ID: %w", err)
	}
	return s.GetUser(ctx, id)
}

func (s *Store) UpdateUser(ctx context.Context, id int64, p UserPatch) (User, error) {
	if _, err := s.GetUser(ctx, id); err != nil {
		return User{}, err
	}
	sets, args := []string{}, []any{}
	if p.DisplayName != nil {
		sets, args = append(sets, "display_name=?"), append(args, *p.DisplayName)
	}
	if p.Role != nil {
		if !validRole(*p.Role) {
			return User{}, fmt.Errorf("role 必须是 admin/member/viewer")
		}
		sets, args = append(sets, "role=?"), append(args, *p.Role)
	}
	if p.Enabled != nil {
		sets, args = append(sets, "enabled=?"), append(args, boolInt(*p.Enabled))
	}
	if p.MemberID != nil {
		if *p.MemberID == 0 {
			sets = append(sets, "member_id=NULL")
		} else {
			sets, args = append(sets, "member_id=?"), append(args, *p.MemberID)
		}
	}
	if p.Password != nil {
		if err := validatePassword(*p.Password); err != nil {
			return User{}, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*p.Password), bcrypt.DefaultCost)
		if err != nil {
			return User{}, fmt.Errorf("生成密码哈希: %w", err)
		}
		sets, args = append(sets, "password_hash=?"), append(args, string(hash))
	}
	if len(sets) == 0 {
		return s.GetUser(ctx, id)
	}
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return User{}, fmt.Errorf("更新用户: %w", err)
	}
	// 停用账号立即吊销其全部会话
	if p.Enabled != nil && !*p.Enabled {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return User{}, fmt.Errorf("吊销会话: %w", err)
		}
	}
	return s.GetUser(ctx, id)
}

// EnsureBootstrapAdmin：users 为空且配置了 FW_AUTH_PASSWORD 时自动创建首个管理员。
// 未配置则保持「安装模式」，由 POST /auth/setup 交互式创建。
func (s *Store) EnsureBootstrapAdmin(ctx context.Context, password string) (bool, error) {
	n, err := s.CountUsers(ctx)
	if err != nil || n > 0 || password == "" {
		return false, err
	}
	if err := validatePassword(password); err != nil {
		return false, fmt.Errorf("FW_AUTH_PASSWORD 不合法: %w", err)
	}
	_, err = s.CreateUser(ctx, "admin", "管理员", password, "admin", nil)
	return err == nil, err
}

// Authenticate：恒定时间比对 + 失败计数锁定，不区分「用户不存在」与「密码错误」以免枚举用户名。
func (s *Store) Authenticate(ctx context.Context, username, password string) (User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+`,password_hash,failed_attempts,locked_until
		FROM users WHERE username=?`, username)
	var u User
	var memberID sql.NullInt64
	var enabled, failed int
	var lastLogin, lockedUntil sql.NullString
	var hash string
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &memberID, &enabled,
		&u.CreatedAt, &lastLogin, &hash, &failed, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrBadCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("读取认证信息: %w", err)
	}
	if lockedUntil.Valid && lockedUntil.String > nowStr() {
		return User{}, ErrAccountLocked
	}
	if enabled == 0 {
		return User{}, ErrAccountDisabled
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		next := failed + 1
		if next >= maxFailedAttempts {
			// 命中阈值当次即返回锁定，避免多一次请求才生效
			v := time.Now().UTC().Add(lockDuration).Format(time.RFC3339)
			if _, err := s.db.ExecContext(ctx,
				`UPDATE users SET failed_attempts=0, locked_until=? WHERE id=?`,
				v, u.ID); err != nil {
				return User{}, fmt.Errorf("记录账号锁定: %w", err)
			}
			return User{}, ErrAccountLocked
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE users SET failed_attempts=? WHERE id=?`, next, u.ID); err != nil {
			return User{}, fmt.Errorf("记录登录失败: %w", err)
		}
		return User{}, ErrBadCredentials
	}
	now := nowStr()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET failed_attempts=0, locked_until=NULL, last_login_at=? WHERE id=?`,
		now, u.ID); err != nil {
		return User{}, fmt.Errorf("更新登录信息: %w", err)
	}
	if memberID.Valid {
		v := memberID.Int64
		u.MemberID = &v
	}
	u.Enabled, u.LastLoginAt = true, &now
	return u, nil
}

// ---------------- sessions ----------------

// CreateSession 返回明文会话 ID（只进 Cookie）与 CSRF token（前端随写请求回传）。
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, string, error) {
	raw, csrf := newSecret(32), newSecret(32)
	now := time.Now().UTC()
	expires := now.Add(sessionTTL).Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowStr()); err != nil {
		return "", "", fmt.Errorf("清理过期会话: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(id_hash,user_id,csrf_token,created_at,expires_at,last_seen_at) VALUES(?,?,?,?,?,?)`,
		sessionHash(raw), userID, csrf, nowStr(), expires, nowStr())
	if err != nil {
		return "", "", fmt.Errorf("创建会话: %w", err)
	}
	return raw, csrf, nil
}

// ResolveSession 校验会话并滑动续期，返回用户与该会话的 CSRF token。
func (s *Store) ResolveSession(ctx context.Context, raw string) (User, string, error) {
	if raw == "" {
		return User{}, "", ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT
		u.id,u.username,u.display_name,u.role,u.member_id,u.enabled,u.created_at,u.last_login_at,
		s.csrf_token,s.expires_at
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.id_hash=? AND s.expires_at > ? AND u.enabled=1`, sessionHash(raw), nowStr())
	var u User
	var memberID sql.NullInt64
	var enabled int
	var lastLogin sql.NullString
	var csrf, expires string
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &memberID, &enabled,
		&u.CreatedAt, &lastLogin, &csrf, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("解析会话: %w", err)
	}
	if memberID.Valid {
		v := memberID.Int64
		u.MemberID = &v
	}
	if lastLogin.Valid {
		v := lastLogin.String
		u.LastLoginAt = &v
	}
	u.Enabled = true
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at=?, expires_at=? WHERE id_hash=?`,
		nowStr(), now.Add(sessionTTL).Format(time.RFC3339), sessionHash(raw)); err != nil {
		return User{}, "", fmt.Errorf("续期会话: %w", err)
	}
	return u, csrf, nil
}

func (s *Store) DeleteSession(ctx context.Context, raw string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash=?`, sessionHash(raw))
	if err != nil {
		return fmt.Errorf("删除会话: %w", err)
	}
	return nil
}
