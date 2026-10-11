// SY Note Sync 服务端 - 版本与常量
package main

const (
	// ServerVersion 服务端版本号（管理后台「关于」与 /api/version 会读它）
	ServerVersion = "2.1.0"

	// ProtocolVersion 同步协议版本。插件与服务端会互相校验，避免版本错配导致静默失败
	ProtocolVersion = 2

	// MinPluginVersion 服务端要求的最低插件版本
	MinPluginVersion = "2.0.0"

	// MaxPullLimit 单次增量拉取最多返回的变更条数（防止一次拉爆内存）
	MaxPullLimit = 5000

	// MaxLogLimit 管理后台日志分页上限
	MaxLogLimit = 200
)
