package main

// 后端存储层：DDL / 迁移 / Repository。
// 设计要点：所有模块共用 items 表，个性字段放 payload(JSON)；
// 新增模块只写 modules 表，不改 DDL、不改本文件的任何查询。
// 用户/会话是身份维度，独立建表（见 auth_store.go），不与 members 混同。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，免 CGO，便于交叉编译到 ARM NAS / 树莓派
)

var (
	ErrNotFound = errors.New("资源不存在")
	ErrConflict = errors.New("资源已存在")
)

const dateLayout = "2006-01-02"

// ---------------- 领域模型 ----------------

// Field 是 schema_json 的最小单元，同时驱动后端校验与前端表单渲染。
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // text|textarea|date|number|select|member
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
	Core     bool     `json:"core,omitempty"` // 通用列，存放在 items 表字段而非 payload
}

type Module struct {
	Key          string  `json:"key"`
	Name         string  `json:"name"`
	Icon         string  `json:"icon"`
	ViewType     string  `json:"view_type"` // calendar|list|timeline|board|chart|users
	Desc         string  `json:"desc"`
	Schema       []Field `json:"schema"`
	Adapter      string  `json:"adapter"`
	RequiredRole string  `json:"required_role"` // viewer|member|admin，模块级可见性
	SortOrder    int     `json:"sort_order"`
	Enabled      bool    `json:"enabled"`
	Builtin      bool    `json:"builtin"`
	CreatedAt    string  `json:"created_at"`
}

type Member struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Color     string  `json:"color"`
	Avatar    *string `json:"avatar"`
	CreatedAt string  `json:"created_at"`
}

type Item struct {
	ID        string         `json:"id"`
	ModuleKey string         `json:"module_key"`
	MemberID  *int64         `json:"member_id"` // nil = 全家
	Title     string         `json:"title"`
	OccurAt   *string        `json:"occur_at"` // YYYY-MM-DD
	EndAt     *string        `json:"end_at"`
	Status    string         `json:"status"`
	Payload   map[string]any `json:"payload"`
	CreatedBy *int64         `json:"created_by"` // 审计：创建者 user_id，系统种子为 NULL
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// ItemPatch 用指针区分「未传」与「零值」。
// MemberID 传 0 表示改回「全家」(NULL)，见 UpdateItem。
type ItemPatch struct {
	Title    *string         `json:"title"`
	OccurAt  *string         `json:"occur_at"`
	EndAt    *string         `json:"end_at"`
	Status   *string         `json:"status"`
	MemberID *int64          `json:"member_id"`
	Payload  *map[string]any `json:"payload"`
}

type ItemFilter struct {
	From, To, Module, Status, Q string
	MemberID                    *int64
	Limit, Offset               int
}

type Overview struct {
	Date  string `json:"date"`
	Days  int    `json:"days"`
	Stats struct {
		Today          int `json:"today"`
		OpenTodo       int `json:"open_todo"`
		UpcomingVisits int `json:"upcoming_visits"`
	} `json:"stats"`
	TodayItems []Item `json:"today_items"`
	Upcoming   []Item `json:"upcoming"`
	OpenTodos  []Item `json:"open_todos"`
}

type Store struct {
	db   *sql.DB
	path string
}

// ---------------- 连接与迁移 ----------------

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	// WAL 让读写并发；busy_timeout 避免偶发写冲突；foreign_keys 保证 module_key / user_id 有效。
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite: %w", err)
	}
	// SQLite 是单写者：单连接串行化所有访问，从根上消除 SQLITE_BUSY，家庭量级无性能问题。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 sqlite 失败: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var ddl = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS modules (
		key           TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		icon          TEXT NOT NULL DEFAULT 'grid',
		view_type     TEXT NOT NULL DEFAULT 'list',
		desc          TEXT NOT NULL DEFAULT '',
		schema_json   TEXT NOT NULL DEFAULT '[]',
		adapter       TEXT NOT NULL DEFAULT 'generic',
		required_role TEXT NOT NULL DEFAULT 'viewer',
		sort_order    INTEGER NOT NULL DEFAULT 0,
		enabled       INTEGER NOT NULL DEFAULT 1,
		builtin       INTEGER NOT NULL DEFAULT 0,
		created_at    TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS members (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL,
		color      TEXT NOT NULL DEFAULT '#5b7cfa',
		avatar     TEXT,
		created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS items (
		id          TEXT PRIMARY KEY,
		module_key  TEXT NOT NULL REFERENCES modules(key),
		member_id   INTEGER REFERENCES members(id),
		title       TEXT NOT NULL,
		occur_at    TEXT,
		end_at      TEXT,
		status      TEXT NOT NULL DEFAULT 'open',
		payload     TEXT NOT NULL DEFAULT '{}',
		created_by  INTEGER REFERENCES users(id),
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL,
		deleted_at  TEXT)`,
	`CREATE INDEX IF NOT EXISTS idx_items_module_time ON items(module_key, occur_at)`,
	`CREATE INDEX IF NOT EXISTS idx_items_member_time ON items(member_id, occur_at)`,
	`CREATE INDEX IF NOT EXISTS idx_items_status ON items(module_key, status) WHERE deleted_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS idx_items_time ON items(occur_at) WHERE deleted_at IS NULL`,
	`CREATE TABLE IF NOT EXISTS reminders (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id TEXT NOT NULL REFERENCES items(id),
		fire_at TEXT NOT NULL,
		channel TEXT NOT NULL DEFAULT 'webpush',
		sent    INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS idx_reminders_fire ON reminders(fire_at, sent)`,
	`CREATE TABLE IF NOT EXISTS users (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		username        TEXT NOT NULL UNIQUE,
		display_name    TEXT NOT NULL,
		password_hash   TEXT NOT NULL,
		role            TEXT NOT NULL DEFAULT 'member',
		member_id       INTEGER REFERENCES members(id),
		enabled         INTEGER NOT NULL DEFAULT 1,
		failed_attempts INTEGER NOT NULL DEFAULT 0,
		locked_until    TEXT,
		created_at      TEXT NOT NULL,
		last_login_at   TEXT)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		id_hash     TEXT PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		csrf_token  TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		expires_at  TEXT NOT NULL,
		last_seen_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at)`,
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移事务: %w", err)
	}
	defer tx.Rollback() // Commit 后 Rollback 返回 ErrTxDone，忽略即可

	for i, stmt := range ddl {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行 DDL[%d]: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_version(version, applied_at) VALUES(?, ?)`, len(ddl), nowStr()); err != nil {
		return fmt.Errorf("写入 schema_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移: %w", err)
	}
	// 兼容 v1 已存在的库：CREATE TABLE IF NOT EXISTS 不会给旧表补列。
	if err := s.ensureColumns(ctx); err != nil {
		return err
	}
	return s.seed(ctx)
}

func (s *Store) ensureColumns(ctx context.Context) error {
	cols := []struct{ table, col, stmt string }{
		{"modules", "required_role", `ALTER TABLE modules ADD COLUMN required_role TEXT NOT NULL DEFAULT 'viewer'`},
		{"items", "created_by", `ALTER TABLE items ADD COLUMN created_by INTEGER`},
	}
	for _, c := range cols {
		rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+c.table+`)`)
		if err != nil {
			return fmt.Errorf("读取表结构(%s): %w", c.table, err)
		}
		found := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("扫描表结构(%s): %w", c.table, err)
			}
			if name == c.col {
				found = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("遍历表结构(%s): %w", c.table, err)
		}
		rows.Close()
		if !found {
			if _, err := s.db.ExecContext(ctx, c.stmt); err != nil {
				return fmt.Errorf("新增列 %s.%s: %w", c.table, c.col, err)
			}
		}
	}
	return nil
}

// ---------------- 种子数据 ----------------

func coreSchema() []Field {
	return []Field{
		{Key: "title", Label: "标题", Type: "text", Required: true, Core: true},
		{Key: "occur_at", Label: "日期", Type: "date", Required: true, Core: true},
		{Key: "member_id", Label: "归属成员", Type: "member", Core: true},
	}
}

func builtinModules() []Module {
	extra := func(fs ...Field) []Field { return append(coreSchema(), fs...) }
	mods := []Module{
		{
			Key: "calendar", Name: "日历", Icon: "calendar", ViewType: "calendar",
			Desc: "记录家庭重要事情、纪念日与安排", SortOrder: 1, Enabled: true, Builtin: true,
			Schema: extra(
				Field{Key: "location", Label: "地点", Type: "text"},
				Field{Key: "note", Label: "备注", Type: "textarea"},
			),
		},
		{
			Key: "todo", Name: "待办", Icon: "todo", ViewType: "list",
			Desc: "家务、缴费、采购，可勾选完成", SortOrder: 2, Enabled: true, Builtin: true,
			Schema: extra(
				Field{Key: "priority", Label: "优先级", Type: "select", Options: []string{"P0", "P1", "P2", "P3"}},
				Field{Key: "note", Label: "备注", Type: "textarea"},
			),
		},
		{
			Key: "health", Name: "健康", Icon: "health", ViewType: "timeline",
			Desc: "吃饭 / 生病 / 哪天要去医院", SortOrder: 3, Enabled: true, Builtin: true,
			Schema: extra(
				Field{Key: "kind", Label: "健康类型", Type: "select", Options: []string{"吃饭", "生病", "就医"}},
				Field{Key: "detail", Label: "详情", Type: "text"},
				Field{Key: "note", Label: "备注", Type: "textarea"},
			),
		},
		{
			// 用户模块是注册表里的普通模块，但数据走独立 users 表（adapter=users），
			// required_role=admin 让非管理员的导航里根本不出现入口。
			Key: "users", Name: "用户与权限", Icon: "settings", ViewType: "users",
			Desc: "账号、角色与登录会话管理（仅管理员可见）", SortOrder: 9,
			RequiredRole: "admin", Adapter: "users", Enabled: true, Builtin: true,
			Schema: []Field{},
		},
	}
	for i := range mods {
		if mods[i].RequiredRole == "" {
			mods[i].RequiredRole = "viewer"
		}
		if mods[i].Adapter == "" {
			mods[i].Adapter = "generic"
		}
	}
	return mods
}

func (s *Store) seed(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始种子事务: %w", err)
	}
	defer tx.Rollback()

	for _, m := range builtinModules() {
		schemaJSON, err := json.Marshal(m.Schema)
		if err != nil {
			return fmt.Errorf("序列化 schema(%s): %w", m.Key, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO modules
			(key,name,icon,view_type,desc,schema_json,adapter,required_role,sort_order,enabled,builtin,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.Key, m.Name, m.Icon, m.ViewType, m.Desc, string(schemaJSON), m.Adapter,
			m.RequiredRole, m.SortOrder, boolInt(m.Enabled), boolInt(m.Builtin), nowStr()); err != nil {
			return fmt.Errorf("写入模块(%s): %w", m.Key, err)
		}
	}
	for _, mb := range []Member{
		{Name: "爸爸", Color: "#5b7cfa"}, {Name: "妈妈", Color: "#f59e0b"},
		{Name: "小宝", Color: "#ec4899"}, {Name: "外婆", Color: "#10b981"},
	} {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE name=?`, mb.Name).Scan(&n); err != nil {
			return fmt.Errorf("查询成员: %w", err)
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO members(name,color,created_at) VALUES(?,?,?)`,
				mb.Name, mb.Color, nowStr()); err != nil {
				return fmt.Errorf("写入成员: %w", err)
			}
		}
	}
	// 示例条目只在 items 为空时写入，避免重启重复插入。
	var itemCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&itemCount); err != nil {
		return fmt.Errorf("统计条目: %w", err)
	}
	if itemCount == 0 {
		for _, it := range seedItems() {
			if err := insertItemTx(ctx, tx, &it); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func seedItems() []Item {
	base := time.Now()
	d := func(n int) *string {
		v := base.AddDate(0, 0, n).Format(dateLayout)
		return &v
	}
	mem := func(n int64) *int64 { return &n }
	p := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	return []Item{
		{ModuleKey: "calendar", MemberID: mem(4), Title: "外婆生日 · 家庭聚餐", OccurAt: d(0), Status: "open", Payload: p("location", "外婆家", "note", "订蛋糕，18:30 到")},
		{ModuleKey: "calendar", MemberID: mem(1), Title: "小宝家长会", OccurAt: d(1), Status: "open", Payload: p("location", "学校三楼报告厅")},
		{ModuleKey: "todo", MemberID: mem(2), Title: "交本月电费", OccurAt: d(0), Status: "open", Payload: p("priority", "P0")},
		{ModuleKey: "todo", MemberID: mem(1), Title: "换净水器滤芯", OccurAt: d(-3), Status: "open", Payload: p("priority", "P1", "note", "已逾期")},
		{ModuleKey: "todo", MemberID: mem(2), Title: "预约空调清洗", OccurAt: d(4), Status: "open", Payload: p("priority", "P3")},
		{ModuleKey: "health", MemberID: mem(3), Title: "带小宝去儿童医院复查", OccurAt: d(0), Status: "open", Payload: p("kind", "就医", "detail", "儿科 · 呼吸科")},
		{ModuleKey: "health", MemberID: mem(3), Title: "小宝疫苗接种（第二针）", OccurAt: d(3), Status: "open", Payload: p("kind", "就医", "detail", "社区卫生服务中心")},
		{ModuleKey: "health", MemberID: mem(4), Title: "外婆血压复诊", OccurAt: d(6), Status: "open", Payload: p("kind", "就医", "detail", "心内科，空腹")},
		{ModuleKey: "health", MemberID: mem(3), Title: "感冒低烧 37.8℃", OccurAt: d(-2), Status: "open", Payload: p("kind", "生病", "detail", "多喝水，观察体温")},
		{ModuleKey: "health", Title: "早餐：燕麦粥、鸡蛋、牛奶", OccurAt: d(0), Status: "done", Payload: p("kind", "吃饭")},
	}
}

// ---------------- modules ----------------

const moduleColumns = `key,name,icon,view_type,desc,schema_json,adapter,required_role,sort_order,enabled,builtin,created_at`

type scanner interface{ Scan(dest ...any) error }

func scanModule(sc scanner) (Module, error) {
	var m Module
	var schemaJSON, desc string
	var enabled, builtin int
	err := sc.Scan(&m.Key, &m.Name, &m.Icon, &m.ViewType, &desc, &schemaJSON,
		&m.Adapter, &m.RequiredRole, &m.SortOrder, &enabled, &builtin, &m.CreatedAt)
	if err != nil {
		return m, err
	}
	m.Desc, m.Enabled, m.Builtin = desc, enabled != 0, builtin != 0
	m.Schema = []Field{}
	if schemaJSON != "" {
		if err := json.Unmarshal([]byte(schemaJSON), &m.Schema); err != nil {
			return m, fmt.Errorf("解析 schema_json(%s): %w", m.Key, err)
		}
	}
	return m, nil
}

// ListModules 按角色过滤 required_role：导航本身就是权限边界，前端无需硬编码隐藏逻辑。
func (s *Store) ListModules(ctx context.Context, role string) ([]Module, error) {
	allowed := rolesUpTo(role)
	ph := make([]string, len(allowed))
	args := make([]any, len(allowed))
	for i, r := range allowed {
		ph[i], args[i] = "?", r
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+moduleColumns+` FROM modules WHERE enabled=1 AND required_role IN (`+
			strings.Join(ph, ",")+`) ORDER BY sort_order, key`, args...)
	if err != nil {
		return nil, fmt.Errorf("查询模块: %w", err)
	}
	defer rows.Close()
	out := []Module{}
	for rows.Next() {
		m, err := scanModule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历模块: %w", err)
	}
	return out, nil
}

func (s *Store) GetModule(ctx context.Context, key string) (Module, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+moduleColumns+` FROM modules WHERE key=?`, key)
	m, err := scanModule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("查询模块(%s): %w", key, err)
	}
	return m, nil
}

func (s *Store) CreateModule(ctx context.Context, m *Module) error {
	if m.Adapter == "" {
		m.Adapter = "generic"
	}
	if m.RequiredRole == "" {
		m.RequiredRole = "viewer"
	}
	if m.CreatedAt == "" {
		m.CreatedAt = nowStr()
	}
	schemaJSON, err := json.Marshal(m.Schema)
	if err != nil {
		return fmt.Errorf("序列化 schema: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO modules
		(key,name,icon,view_type,desc,schema_json,adapter,required_role,sort_order,enabled,builtin,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Key, m.Name, m.Icon, m.ViewType, m.Desc, string(schemaJSON), m.Adapter,
		m.RequiredRole, m.SortOrder, boolInt(m.Enabled), boolInt(m.Builtin), m.CreatedAt)
	if err != nil {
		if isUniqueErr(err) {
			return ErrConflict
		}
		return fmt.Errorf("写入模块: %w", err)
	}
	return nil
}

func (s *Store) UpdateModule(ctx context.Context, key string, enabled *bool, sortOrder *int, name *string) (Module, error) {
	m, err := s.GetModule(ctx, key)
	if err != nil {
		return m, err
	}
	if enabled != nil {
		m.Enabled = *enabled
	}
	if sortOrder != nil {
		m.SortOrder = *sortOrder
	}
	if name != nil {
		m.Name = *name
	}
	_, err = s.db.ExecContext(ctx, `UPDATE modules SET enabled=?, sort_order=?, name=? WHERE key=?`,
		boolInt(m.Enabled), m.SortOrder, m.Name, key)
	if err != nil {
		return m, fmt.Errorf("更新模块: %w", err)
	}
	return m, nil
}

// ---------------- members ----------------

func (s *Store) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,color,avatar,created_at FROM members ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询成员: %w", err)
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var avatar sql.NullString
		if err := rows.Scan(&m.ID, &m.Name, &m.Color, &avatar, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描成员: %w", err)
		}
		if avatar.Valid {
			v := avatar.String
			m.Avatar = &v
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历成员: %w", err)
	}
	return out, nil
}

// ---------------- items ----------------

const itemColumns = `id,module_key,member_id,title,occur_at,end_at,status,payload,created_at,updated_at,created_by`

func scanItem(sc scanner) (Item, error) {
	var it Item
	var memberID, createdBy sql.NullInt64
	var occurAt, endAt sql.NullString
	var payload string
	err := sc.Scan(&it.ID, &it.ModuleKey, &memberID, &it.Title, &occurAt, &endAt,
		&it.Status, &payload, &it.CreatedAt, &it.UpdatedAt, &createdBy)
	if err != nil {
		return it, err
	}
	if memberID.Valid {
		v := memberID.Int64
		it.MemberID = &v
	}
	if occurAt.Valid {
		v := occurAt.String
		it.OccurAt = &v
	}
	if endAt.Valid {
		v := endAt.String
		it.EndAt = &v
	}
	if createdBy.Valid {
		v := createdBy.Int64
		it.CreatedBy = &v
	}
	it.Payload = map[string]any{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &it.Payload); err != nil {
			return it, fmt.Errorf("解析 payload(%s): %w", it.ID, err)
		}
	}
	return it, nil
}

// buildWhere 只拼白名单列名，所有值一律参数化，杜绝注入。
func buildWhere(f ItemFilter) (string, []any) {
	conds := []string{"deleted_at IS NULL"}
	args := []any{}
	if f.Module != "" {
		conds = append(conds, "module_key = ?")
		args = append(args, f.Module)
	}
	if f.From != "" {
		conds = append(conds, "occur_at >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "occur_at <= ?")
		args = append(args, f.To)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.MemberID != nil {
		// 全家条目 (member_id IS NULL) 在任何成员视图下都可见
		conds = append(conds, "(member_id = ? OR member_id IS NULL)")
		args = append(args, *f.MemberID)
	}
	if f.Q != "" {
		conds = append(conds, "(title LIKE ? OR payload LIKE ?)")
		like := "%" + f.Q + "%"
		args = append(args, like, like)
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func (s *Store) ListItems(ctx context.Context, f ItemFilter) ([]Item, int, error) {
	where, args := buildWhere(f)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计条目: %w", err)
	}
	query := `SELECT ` + itemColumns + ` FROM items ` + where +
		` ORDER BY (occur_at IS NULL), occur_at, created_at LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询条目: %w", err)
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历条目: %w", err)
	}
	return out, total, nil
}

func (s *Store) GetItem(ctx context.Context, id string) (Item, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE id=? AND deleted_at IS NULL`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	if err != nil {
		return it, fmt.Errorf("查询条目(%s): %w", id, err)
	}
	return it, nil
}

func insertItemTx(ctx context.Context, tx *sql.Tx, it *Item) error {
	if it.ID == "" {
		it.ID = newID()
	}
	if it.Status == "" {
		it.Status = "open"
	}
	if it.Payload == nil {
		it.Payload = map[string]any{}
	}
	if it.CreatedAt == "" {
		it.CreatedAt = nowStr()
	}
	it.UpdatedAt = it.CreatedAt
	payload, err := json.Marshal(it.Payload)
	if err != nil {
		return fmt.Errorf("序列化 payload: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO items
		(id,module_key,member_id,title,occur_at,end_at,status,payload,created_at,updated_at,created_by)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, it.ModuleKey, nullInt(it.MemberID), it.Title, nullStr(it.OccurAt),
		nullStr(it.EndAt), it.Status, string(payload), it.CreatedAt, it.UpdatedAt,
		nullInt(it.CreatedBy))
	if err != nil {
		return fmt.Errorf("写入条目: %w", err)
	}
	return nil
}

func (s *Store) CreateItem(ctx context.Context, it *Item) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始写事务: %w", err)
	}
	defer tx.Rollback()
	if err := insertItemTx(ctx, tx, it); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateItem(ctx context.Context, id string, p ItemPatch) (Item, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, fmt.Errorf("开始更新事务: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE id=? AND deleted_at IS NULL`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("读取待更新条目: %w", err)
	}
	if p.Title != nil {
		it.Title = *p.Title
	}
	if p.OccurAt != nil {
		it.OccurAt = p.OccurAt
	}
	if p.EndAt != nil {
		it.EndAt = p.EndAt
	}
	if p.Status != nil {
		it.Status = *p.Status
	}
	if p.MemberID != nil {
		if *p.MemberID == 0 {
			it.MemberID = nil // 0 = 全家
		} else {
			v := *p.MemberID
			it.MemberID = &v
		}
	}
	if p.Payload != nil {
		merged := map[string]any{}
		for k, v := range it.Payload {
			merged[k] = v
		}
		for k, v := range *p.Payload {
			merged[k] = v
		}
		it.Payload = merged
	}
	it.UpdatedAt = nowStr()
	payload, err := json.Marshal(it.Payload)
	if err != nil {
		return Item{}, fmt.Errorf("序列化 payload: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE items SET title=?,occur_at=?,end_at=?,status=?,member_id=?,payload=?,updated_at=? WHERE id=? AND deleted_at IS NULL`,
		it.Title, nullStr(it.OccurAt), nullStr(it.EndAt), it.Status, nullInt(it.MemberID),
		string(payload), it.UpdatedAt, it.ID)
	if err != nil {
		return Item{}, fmt.Errorf("更新条目: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("提交更新: %w", err)
	}
	return it, nil
}

func (s *Store) SoftDeleteItem(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE items SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		nowStr(), nowStr(), id)
	if err != nil {
		return fmt.Errorf("软删除条目: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------- 首页跨模块聚合 ----------------

func (s *Store) Overview(ctx context.Context, date string, days int) (Overview, error) {
	if date == "" {
		date = time.Now().Format(dateLayout)
	}
	if days <= 0 || days > 31 {
		days = 7
	}
	end, err := addDays(date, days)
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{Date: date, Days: days}
	ov.TodayItems, ov.Stats.Today, err = s.ListItems(ctx, ItemFilter{From: date, To: date, Limit: 200})
	if err != nil {
		return ov, fmt.Errorf("聚合今日: %w", err)
	}
	nextDay, err := addDays(date, 1)
	if err != nil {
		return ov, err
	}
	ov.Upcoming, _, err = s.ListItems(ctx, ItemFilter{From: nextDay, To: end, Limit: 200})
	if err != nil {
		return ov, fmt.Errorf("聚合未来: %w", err)
	}
	ov.OpenTodos, ov.Stats.OpenTodo, err = s.ListItems(ctx, ItemFilter{Module: "todo", Status: "open", Limit: 5})
	if err != nil {
		return ov, fmt.Errorf("聚合待办: %w", err)
	}
	for _, it := range ov.Upcoming {
		if it.ModuleKey == "health" && it.Payload["kind"] == "就医" {
			ov.Stats.UpcomingVisits++
		}
	}
	return ov, nil
}

// ---------------- 备份 ----------------

// Backup 使用 SQLite 官方在线备份方式 VACUUM INTO，WAL 模式下安全，无需 sqlite3 CLI。
func (s *Store) Backup(ctx context.Context, dest string) (string, error) {
	if dest == "" {
		dest = filepath.Join(filepath.Dir(s.path), "backup",
			"family-"+time.Now().Format("20060102-150405")+".db")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "", fmt.Errorf("创建备份目录: %w", err)
	}
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("备份文件已存在: %s", dest)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("在线备份失败: %w", err)
	}
	return dest, nil
}

// ---------------- 工具函数 ----------------

func nowStr() string { return time.Now().UTC().Format(time.RFC3339) }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("i%d", time.Now().UnixNano())
	}
	return "i" + hex.EncodeToString(b[:])
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func isUniqueErr(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint") ||
		strings.Contains(strings.ToLower(err.Error()), "primary key")
}

func addDays(date string, n int) (string, error) {
	t, err := time.ParseInLocation(dateLayout, date, time.Local)
	if err != nil {
		return "", fmt.Errorf("日期格式应为 YYYY-MM-DD: %w", err)
	}
	return t.AddDate(0, 0, n).Format(dateLayout), nil
}

func parseDate(s string) error {
	_, err := time.Parse(dateLayout, s)
	return err
}

func parseMember(raw string) (*int64, error) {
	if raw == "" || raw == "all" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return nil, fmt.Errorf("member 必须是正整数或 all")
	}
	return &v, nil
}
