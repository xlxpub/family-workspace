package main

// 启动入口：单进程单二进制，API 与前端静态页共用 8080。
// 运行：go run .   （或 CGO_ENABLED=0 go build -o family-server . && ./family-server）
// 环境变量：
//   FW_DB              默认 ./data/family.db
//   FW_ADDR            默认 :8080
//   FW_AUTH_PASSWORD   初始管理员密码 + 脚本 API Token；为空则进入安装模式(POST /auth/setup)
//   FW_COOKIE_SECURE   =1 时给会话 Cookie 加 Secure（HTTPS 反代场景）

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed static/index.html
var staticFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dbPath := env("FW_DB", "./data/family.db")
	addr := env("FW_ADDR", ":8080")
	password := os.Getenv("FW_AUTH_PASSWORD")
	cookieSecure := os.Getenv("FW_COOKIE_SECURE") == "1"
	log.Printf("测试自动发布4 2")

	st, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	defer st.Close()

	// users 表为空时自动创建首个管理员；未配置口令则保持安装模式。
	if createdAdmin, err := st.EnsureBootstrapAdmin(context.Background(), password); err != nil {
		log.Fatalf("初始化管理员失败: %v", err)
	} else if createdAdmin {
		log.Printf("已根据 FW_AUTH_PASSWORD 创建首个管理员账号 admin")
	}

	html, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		log.Fatalf("读取内嵌前端失败: %v", err)
	}
	ai, err := newAIClient(env("FW_AI_BASE_URL", "https://api.deepseek.com"), os.Getenv("FW_AI_API_KEY"), env("FW_AI_MODEL", "deepseek-chat"))
	if err != nil {
		log.Fatalf("模型配置不合法: %v", err)
	}
	api := &API{store: st, password: password, cookieSecure: cookieSecure, indexHTML: html, ai: ai}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 单进程内扫提醒：多副本部署会重复触发，须改用文件锁或独立进程。
	go startReminderLoop(ctx, st)
	go func() {
		<-ctx.Done()
		log.Println("收到退出信号，正在优雅关闭…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("优雅关闭失败: %v", err)
		}
	}()

	log.Printf("家庭工作台已启动: http://%s  (db=%s, api_token=%v, cookie_secure=%v)",
		addr, dbPath, password != "", cookieSecure)
	if password == "" {
		log.Printf("当前无用户账号，处于安装模式：POST /api/v1/auth/setup 创建首个管理员")
	}
	// 启动即打印实际生效的模型名：模型 ID 写错时上游只会返回 400，提前暴露可省去排查。
	if ai != nil {
		log.Printf("一句话记录已启用: model=%s endpoint=%s", ai.model, ai.endpoint)
	} else {
		log.Printf("未配置 FW_AI_API_KEY，一句话记录已关闭")
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务异常退出: %v", err)
	}
}

func startReminderLoop(ctx context.Context, st *Store) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			from := time.Now().Format(dateLayout)
			to, err := addDays(from, 1)
			if err != nil {
				continue
			}
			items, _, err := st.ListItems(ctx, ItemFilter{From: from, To: to, Status: "open", Limit: 200})
			if err != nil {
				log.Printf("扫描提醒失败: %v", err)
				continue
			}
			if len(items) > 0 {
				log.Printf("[提醒] 未来 24h 有 %d 条待处理事项", len(items))
			}
		}
	}
}
