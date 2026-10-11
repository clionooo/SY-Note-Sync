// SY Note Sync 服务端 - 数据模型
//
// 这里集中定义「线上协议」用到的结构体。
// 与插件通信的 JSON 字段名一旦定下就不要再改，否则老插件会静默同步失败。
package main

import (
	"bytes"
	"encoding/json"
)

// BlockChange 一个块的变更。这是插件与服务端之间最核心的结构。
//
// 注意 Content 字段承载的是**思源块 DOM**（带 data-node-id 的 HTML 片段），
// 不是 markdown。插件写回本地时用 dataType=dom，才能保留块 ID。
// （同步是按块 ID 对账的，丢了 ID 后面全对不上。）
type BlockChange struct {
	BlockID    string `json:"blockId"`              // 思源块 ID
	DocID      string `json:"docId,omitempty"`      // 所属文档 ID
	NotebookID string `json:"notebookId,omitempty"` // 所属笔记本 ID
	Op         string `json:"op"`                   // insert | update | delete
	Content    string `json:"content"`              // 块 DOM（delete 时为空）
	ParentID   string `json:"parentId,omitempty"`   // 父块 ID（insert 定位用）
	BaseRev    int64  `json:"baseRev,omitempty"`    // 本地已知版本号，服务端据此判冲突
	Rev        int64  `json:"rev,omitempty"`        // 服务端分配后的版本号
	// Force=true 表示「以我为准」，服务端跳过冲突判定直接覆盖。
	// 用于插件的「全量上传本机内容」——那是用户主动发起的整体覆盖，不该弹冲突。
	Force bool `json:"force,omitempty"`
}

// UnmarshalJSON 兼容 v1 协议里的 "markdown" 字段名。
// v1 插件发的是 {"markdown": "..."}，v2 改叫 content 更贴切，
// 但老设备不该因为改个名就同步不了，所以两个名字都收。
func (c *BlockChange) UnmarshalJSON(data []byte) error {
	var raw struct {
		BlockID    string `json:"blockId"`
		DocID      string `json:"docId"`
		NotebookID string `json:"notebookId"`
		Op         string `json:"op"`
		Content    string `json:"content"`
		Markdown   string `json:"markdown"` // v1 兼容
		ParentID   string `json:"parentId"`
		BaseRev    int64  `json:"baseRev"`
		Rev        int64  `json:"rev"`
		Force      bool   `json:"force"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	c.BlockID = raw.BlockID
	c.DocID = raw.DocID
	c.NotebookID = raw.NotebookID
	c.Op = raw.Op
	if raw.Content != "" {
		c.Content = raw.Content
	} else {
		c.Content = raw.Markdown
	}
	c.ParentID = raw.ParentID
	c.BaseRev = raw.BaseRev
	c.Rev = raw.Rev
	c.Force = raw.Force
	return nil
}

// Device 一台已注册的设备（手机 / 电脑 / 容器里的思源实例）
type Device struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Key      string `json:"key,omitempty"` // 只在管理后台显式索取时返回
	Platform string `json:"platform,omitempty"`
	Version  string `json:"version,omitempty"`
	Note     string `json:"note,omitempty"`
	Created  int64  `json:"created"`
	LastSeen int64  `json:"lastSeen"`
	Online   bool   `json:"online"`
	// 统计字段（列表接口按需填充）
	ChangeCount int64 `json:"changeCount,omitempty"`
	BlockCount  int64 `json:"blockCount,omitempty"`
}

// LogEntry 一条变更日志（管理后台列表用）
type LogEntry struct {
	ID         int64  `json:"id"`
	BlockID    string `json:"blockId"`
	DocID      string `json:"docId,omitempty"`
	NotebookID string `json:"notebookId,omitempty"`
	Op         string `json:"op"`
	DeviceID   int64  `json:"deviceId"`
	DeviceName string `json:"deviceName,omitempty"`
	Ts         int64  `json:"ts"`
	Content    string `json:"content,omitempty"`
}

// BlockState 服务端保存的块最新状态
type BlockState struct {
	BlockID    string `json:"blockId"`
	DocID      string `json:"docId"`
	NotebookID string `json:"notebookId"`
	Op         string `json:"op"`
	Content    string `json:"content"`
	ParentID   string `json:"parentId"`
	Rev        int64  `json:"rev"`
	DeviceID   int64  `json:"deviceId"`
	Updated    int64  `json:"updated"`
	Deleted    bool   `json:"deleted"`
}

// Conflict 未解决的块冲突：两台设备离线改了同一块
type Conflict struct {
	ID        int64       `json:"id"`
	BlockID   string      `json:"blockId"`
	DocID     string      `json:"docId"`
	Created   int64       `json:"created"`
	VersionA  BlockChange `json:"versionA"` // 服务端当前版本
	VersionB  BlockChange `json:"versionB"` // 后到的挑战版本
	Resolved  bool        `json:"resolved"`
	ChosenRev int64       `json:"chosenRev"`
}

// Snapshot 快照
type Snapshot struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Blocks  int64  `json:"blocks"`
	Created int64  `json:"created"`
	Note    string `json:"note,omitempty"`
	Auto    bool   `json:"auto"`
}

// Share 分享链接
type Share struct {
	ID       int64  `json:"id"`
	Token    string `json:"token"`
	Title    string `json:"title"`
	BlockID  string `json:"blockId,omitempty"`
	DocID    string `json:"docId,omitempty"`
	Password string `json:"password,omitempty"`
	Views    int64  `json:"views"`
	Created  int64  `json:"created"`
	Expires  int64  `json:"expires,omitempty"`
}

// Notebook 笔记本（由插件上报）
type Notebook struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	Docs    int64  `json:"docs"`
	Blocks  int64  `json:"blocks"`
	Updated int64  `json:"updated"`
}

// Doc 文档（由插件上报）
type Doc struct {
	ID         string `json:"id"`
	NotebookID string `json:"notebookId"`
	Title      string `json:"title"`
	Blocks     int64  `json:"blocks"`
	Updated    int64  `json:"updated"`
	Deleted    bool   `json:"deleted"`
}

// Stats 看板统计数据
type Stats struct {
	Devices    int64 `json:"devices"`
	Online     int64 `json:"online"`
	Blocks     int64 `json:"blocks"`
	Deleted    int64 `json:"deleted"`
	Changes    int64 `json:"changes"`
	Conflicts  int64 `json:"conflicts"`
	Snapshots  int64 `json:"snapshots"`
	Notebooks  int64 `json:"notebooks"`
	Docs       int64 `json:"docs"`
	Shares     int64 `json:"shares"`
	Assets     int64 `json:"assets"`
	AssetBytes int64 `json:"assetBytes"`
	DBBytes    int64 `json:"dbBytes"`
	LastChange int64 `json:"lastChange"`
}

// SettingKV 服务端设置项
type SettingKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Asset 附件（图片/音频/视频等非块内容）
//
// Path 是相对于思源工作空间 data 目录的路径，例如 "assets/2026/10/xxx.png"。
// 服务端把文件本体存在 <数据目录>/files/<Path>，数据库只存元信息。
type Asset struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Hash     string `json:"hash,omitempty"` // sha256，用于「内容变了要重传」的判断
	Mime     string `json:"mime,omitempty"`
	DeviceID int64  `json:"deviceId,omitempty"`
	Updated  int64  `json:"updated"`
}
