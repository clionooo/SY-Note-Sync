// SY Note Sync 中心化同步服务
// 架构参考 obsidian-fast-note-sync：所有思源实例只连本服务，服务是唯一枢纽。
package main

import (
	"embed"
	"flag"
	"log"
	"net/http"
	"os"
)

// adminHTML 从二进制里内嵌管理后台页面，单文件即可部署
//
//go:embed web/admin.html
var adminFS embed.FS

// 命令行/环境变量配置
var (
	cfgAddr       string // 监听地址，如 :8282
	cfgDBPath     string // sqlite 数据库文件路径
	cfgDataDir    string // 快照等文件存储目录
	cfgAdminToken string // Web 后台与设备注册的管理令牌
)

func main() {
	// 优先取环境变量（方便 Docker 部署），否则取命令行参数，最后用默认值
	flag.StringVar(&cfgAddr, "addr", envOr("SYNCSRV_ADDR", ":8282"), "HTTP 监听地址")
	flag.StringVar(&cfgDBPath, "db", envOr("SYNCSRV_DB", "data/sync.db"), "SQLite 数据库路径")
	flag.StringVar(&cfgDataDir, "data", envOr("SYNCSRV_DATA", "data"), "快照数据目录")
	flag.StringVar(&cfgAdminToken, "admin-token", envOr("SYNCSRV_ADMIN_TOKEN", "change-me"), "管理令牌（注册设备/进入 Web 后台）")
	flag.Parse()

	// 初始化 SQLite 存储层
	if err := initDB(cfgDBPath); err != nil {
		log.Fatalf("[init] 打开数据库失败: %v", err)
	}
	// 初始化快照目录
	if err := os.MkdirAll(cfgDataDir, 0o755); err != nil {
		log.Fatalf("[init] 创建数据目录失败: %v", err)
	}
	// 初始化 WebSocket 枢纽（管理所有在线设备长连接）
	hubRef = newHub()
	go hubRef.run()

	// ---------- 路由注册 ----------
	// 设备注册（需要管理令牌）
	http.HandleFunc("/api/register", handleRegister)
	// 同步核心接口（设备密钥鉴权）
	http.HandleFunc("/api/push", authDevice(handlePush))    // 推送本地块变更
	http.HandleFunc("/api/pull", authDevice(handlePull))    // 拉取远端块变更（增量）
	http.HandleFunc("/api/full", authDevice(handleFull))    // 全量拉取（首次同步/修复）
	http.HandleFunc("/api/ws", authDevice(handleWS))        // WebSocket 长连接
	// 快照接口
	http.HandleFunc("/api/snapshot/create", authDevice(handleSnapshotCreate)) // 创建快照
	http.HandleFunc("/api/snapshot/list", authDevice(handleSnapshotList))     // 快照列表
	http.HandleFunc("/api/snapshot/rollback", authDevice(handleSnapshotRollback)) // 回滚快照（返回全量块状态）
	// 冲突处理
	http.HandleFunc("/api/conflict/list", authDevice(handleConflictList))    // 未解决冲突列表
	http.HandleFunc("/api/conflict/resolve", authDevice(handleConflictResolve)) // 解决冲突（选定版本）
	// Web 管理后台
	http.HandleFunc("/admin", handleAdminPage)             // 后台页面
	http.HandleFunc("/api/admin/data", authAdmin(handleAdminData)) // 后台数据
	http.HandleFunc("/api/admin/snapshot/create", authAdmin(handleAdminSnapshotCreate)) // 后台创建快照

	// 健康检查
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("[start] SY Note Sync 服务已启动，监听 %s", cfgAddr)
	log.Printf("[start] 管理后台: http://<host>%s/admin  (令牌: %s)", cfgAddr, cfgAdminToken)
	if err := http.ListenAndServe(cfgAddr, nil); err != nil {
		log.Fatalf("[start] 服务退出: %v", err)
	}
}

// envOr 读取环境变量，为空则返回默认值
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
