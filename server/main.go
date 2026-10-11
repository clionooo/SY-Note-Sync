// SY Note Sync 中心化同步服务（服务端）
//
// 架构参考 Fast Note Sync：所有思源实例只连本服务，服务是唯一枢纽，客户端之间零直连。
// 同步单元是思源「块」（Block ID），不是 md 文件。
//
// 目录结构：
//
//	main.go             入口 + 路由 + CORS
//	version.go          版本常量
//	models.go           线上协议结构体
//	store.go            SQLite 存储层（所有 SQL 集中在此）
//	hub.go              WebSocket 枢纽（设备推送 + 后台实时刷新）
//	handlers_sync.go    设备侧接口
//	handlers_admin.go   管理后台接口
//	web/                内嵌的管理后台与分享页（无需外部静态服务器）
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// webFS 把管理后台前端打进二进制，单文件即可部署
//
//go:embed web
var webFS embed.FS

// upgrader WebSocket 升级器。
// CheckOrigin 一律放行：插件可能来自 http://127.0.0.1:6806（iOS）、
// capacitor://localhost、file:// 等各种来源，鉴权靠 device_key / admin token，不靠 Origin。
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// 运行期配置
var (
	cfgAddr       string // 监听地址，如 :58282
	cfgDBPath     string // SQLite 路径
	cfgDataDir    string // 数据目录（快照/导出）
	cfgAdminToken string // 管理令牌
	cfgBaseURL    string // 对外访问地址，用于分享链接拼接（可选）
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveAdminToken 决定本次运行用的管理令牌。
//
//	优先级：命令行/环境变量 > 数据库里存过的 > 新生成的随机令牌
//
// 返回值第二项表示「是这次新生成的」（调用方会把它打印出来）。
// 注意 "change-me" 这种占位值一律当作没配，免得有人照抄 compose 模板就直接上线。
func resolveAdminToken(configured string) (string, bool) {
	configured = strings.TrimSpace(configured)
	if configured != "" && configured != "change-me" {
		return configured, false
	}
	if saved := getSetting("admin_token", ""); saved != "" {
		return saved, false
	}
	tok := randHex(16)
	if err := setSetting("admin_token", tok); err != nil {
		log.Printf("[warn] 保存自动生成的令牌失败（重启后会变）: %v", err)
	}
	return tok, true
}

func main() {
	flag.StringVar(&cfgAddr, "addr", envOr("SYNCSRV_ADDR", ":58282"), "HTTP 监听地址")
	flag.StringVar(&cfgDBPath, "db", envOr("SYNCSRV_DB", "data/sync.db"), "SQLite 数据库路径")
	flag.StringVar(&cfgDataDir, "data", envOr("SYNCSRV_DATA", "data"), "数据目录")
	flag.StringVar(&cfgAdminToken, "admin-token", envOr("SYNCSRV_ADMIN_TOKEN", ""), "管理令牌（注册设备/进入后台）；留空则首次启动自动生成随机令牌")
	flag.StringVar(&cfgBaseURL, "base-url", envOr("SYNCSRV_BASE_URL", ""), "对外访问地址（分享链接用），如 https://sync.example.com")
	flag.Parse()

	if err := initDB(cfgDBPath); err != nil {
		log.Fatalf("[init] 打开数据库失败: %v", err)
	}
	if err := os.MkdirAll(cfgDataDir, 0o755); err != nil {
		log.Fatalf("[init] 创建数据目录失败: %v", err)
	}

	// 管理令牌：显式配置优先；没配就用数据库里存过的；再没有就随机生成一个并持久化。
	// 这样默认部署也是随机的（不会出现人尽皆知的默认口令），重启后还能保持同一个令牌。
	var generated bool
	cfgAdminToken, generated = resolveAdminToken(cfgAdminToken)
	if generated {
		log.Printf("[start] ⚠️ 未配置管理令牌，已自动生成随机令牌：%s", cfgAdminToken)
		log.Printf("[start]    它已存进数据库 settings.admin_token，重启不会变；想换可在 docker-compose.yml 里设 SYNCSRV_ADMIN_TOKEN")
	}

	// 启动时清掉上次运行遗留的「在线」状态
	hubRef = newHub()
	go hubRef.keepAlive()

	mux := http.NewServeMux()

	// ---------- 设备侧 ----------
	mux.HandleFunc("/api/register", handleRegister)
	mux.HandleFunc("/api/hello", authDevice(handleHello))
	mux.HandleFunc("/api/push", authDevice(handlePush))
	mux.HandleFunc("/api/pull", authDevice(handlePull))
	mux.HandleFunc("/api/full", authDevice(handleFull))
	mux.HandleFunc("/api/ws", authDevice(handleWS))
	mux.HandleFunc("/api/meta", authDevice(handleMeta))
	mux.HandleFunc("/api/snapshot/create", authDevice(handleSnapshotCreate))
	mux.HandleFunc("/api/snapshot/list", authDevice(handleSnapshotList))
	mux.HandleFunc("/api/snapshot/rollback", authDevice(handleSnapshotRollback))
	mux.HandleFunc("/api/conflict/list", authDevice(handleConflictList))
	mux.HandleFunc("/api/conflict/resolve", authDevice(handleConflictResolve))
	mux.HandleFunc("/api/trash", authDevice(handleTrash))
	mux.HandleFunc("/api/block/history", authDevice(handleBlockHistory))
	mux.HandleFunc("/api/block/restore", authDevice(handleBlockRestore))
	// 附件（图片/音频/视频）
	mux.HandleFunc("/api/asset/check", authDevice(handleAssetCheck))
	mux.HandleFunc("/api/asset/upload", authDevice(handleAssetUpload))
	mux.HandleFunc("/api/asset/download", authDevice(handleAssetDownload))
	mux.HandleFunc("/api/asset/list", authDevice(handleAssetList))
	mux.HandleFunc("/api/asset/delete", authDevice(handleAssetDelete))

	// ---------- 管理后台 ----------
	mux.HandleFunc("/api/admin/data", authAdmin(handleAdminData))
	mux.HandleFunc("/api/admin/devices", authAdmin(handleAdminDevices))
	mux.HandleFunc("/api/admin/device/update", authAdmin(handleAdminDeviceUpdate))
	mux.HandleFunc("/api/admin/device/delete", authAdmin(handleAdminDeviceDelete))
	mux.HandleFunc("/api/admin/device/rotate", authAdmin(handleAdminDeviceRotate))
	mux.HandleFunc("/api/admin/device/key", authAdmin(handleAdminDeviceKey))
	mux.HandleFunc("/api/admin/logs", authAdmin(handleAdminLogs))
	mux.HandleFunc("/api/admin/block/history", authAdmin(handleAdminBlockHistory))
	mux.HandleFunc("/api/admin/block/restore", authAdmin(handleAdminBlockRestore))
	mux.HandleFunc("/api/admin/trash", authAdmin(handleAdminTrash))
	mux.HandleFunc("/api/admin/conflicts", authAdmin(handleAdminConflicts))
	mux.HandleFunc("/api/admin/conflict/resolve", authAdmin(handleAdminConflictResolve))
	mux.HandleFunc("/api/admin/snapshots", authAdmin(handleAdminSnapshots))
	mux.HandleFunc("/api/admin/snapshot/create", authAdmin(handleAdminSnapshotCreate))
	mux.HandleFunc("/api/admin/snapshot/delete", authAdmin(handleAdminSnapshotDelete))
	mux.HandleFunc("/api/admin/snapshot/rollback", authAdmin(handleAdminSnapshotRollback))
	mux.HandleFunc("/api/admin/notebooks", authAdmin(handleAdminNotebooks))
	mux.HandleFunc("/api/admin/docs", authAdmin(handleAdminDocs))
	mux.HandleFunc("/api/admin/doc/blocks", authAdmin(handleAdminDocBlocks))
	mux.HandleFunc("/api/admin/shares", authAdmin(handleAdminShares))
	mux.HandleFunc("/api/admin/share/create", authAdmin(handleAdminShareCreate))
	mux.HandleFunc("/api/admin/share/delete", authAdmin(handleAdminShareDelete))
	mux.HandleFunc("/api/admin/assets", authAdmin(handleAdminAssets))
	mux.HandleFunc("/api/admin/asset/delete", authAdmin(handleAdminAssetDelete))
	mux.HandleFunc("/api/admin/asset/download", authAdmin(handleAdminAssetDownload))
	mux.HandleFunc("/api/admin/settings", authAdmin(handleAdminSettings))
	mux.HandleFunc("/api/admin/export", authAdmin(handleAdminExport))
	mux.HandleFunc("/api/admin/ws", handleAdminWS)
	mux.HandleFunc("/admin", handleAdminPage)
	mux.HandleFunc("/admin/", handleAdminPage)

	// ---------- 公开 ----------
	mux.HandleFunc("/api/version", handleVersion)
	mux.HandleFunc("/api/share/", handleShareAPI)
	mux.HandleFunc("/share/", handleSharePage)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// 静态资源（admin.css / admin.js）直接从内嵌 FS 取
	// 注意要 StripPrefix，否则会去子 FS 里找 /web/admin.css 而 404
	mux.Handle("/web/", http.StripPrefix("/web/", http.FileServer(http.FS(mustSub(webFS, "web")))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Addr:              cfgAddr,
		Handler:           withCORS(withLogging(mux)),
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout：WebSocket 是长连接，设了会被定期掐断
	}
	log.Printf("[start] SY Note Sync 服务已启动 %s (version %s, protocol %d)", cfgAddr, ServerVersion, ProtocolVersion)
	log.Printf("[start] 管理后台: http://<host>%s/admin", cfgAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[start] 服务退出: %v", err)
	}
}

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		log.Fatalf("[init] 内嵌资源 %s 读取失败: %v", dir, err)
	}
	return sub
}

// serveEmbedded 从内嵌 FS 输出一个 HTML 文件
func serveEmbedded(w http.ResponseWriter, r *http.Request, name string) {
	data, err := webFS.ReadFile(name)
	if err != nil {
		http.Error(w, "资源缺失: "+name, 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// withCORS 给所有响应加跨域头，并处理 OPTIONS 预检。
//
// ⚠️ 这一步是必须的，不是可选优化：
// 插件跑在思源前端（Docker 网页端是 http://host:33666，iOS 是 http://127.0.0.1:6806），
// 与本服务不同源。带自定义头 X-Device-Key 的请求会先发 OPTIONS 预检，
// 没有这些响应头的话浏览器直接 `Failed to fetch` —— 表现是「插件显示已连接，
// 但注册/推送全部失败」，很容易误判成网络问题。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Device-Key, X-Admin-Token")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 记录状态码，供日志中间件使用
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// withLogging 只记 API 请求（静态资源不刷屏），出问题时能在 docker logs 里一眼看到
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// WebSocket 是长连接，记了会误导（时长=连接时长）
		if strings.HasSuffix(r.URL.Path, "/ws") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		// 心跳类接口（hello/pull）正常时不记，避免刷屏
		if rec.code == 200 && (strings.HasSuffix(r.URL.Path, "/hello")) {
			return
		}
		log.Printf("[api] %s %s -> %d (%dms) from %s",
			r.Method, r.URL.Path, rec.code, time.Since(start).Milliseconds(), clientIP(r))
	})
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
