// 存储层：SQLite 保存同步元数据（设备/变更日志/最新块状态/冲突/快照）
// 设计要点：服务端不读写思源数据库，只存插件上报的块级变更。
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log"
	"time"

	// modernc.org/sqlite 是纯 Go 实现的 SQLite 驱动，无需 cgo，方便交叉编译与 Docker 构建
	_ "modernc.org/sqlite"
)

var db *sql.DB

// initDB 打开数据库并建表
func initDB(path string) error {
	var err error
	db, err = sql.Open("sqlite", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return err
	}
	// 设备表：每台设备一个独立密钥
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS devices(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		key TEXT NOT NULL UNIQUE,
		created INTEGER NOT NULL,
		last_seen INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return err
	}
	// 变更日志表：所有设备上报的块变更流水（同步单元是块，不是文件）
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS changes(
		id INTEGER PRIMARY KEY AUTOINCREMENT,   -- 即全局递增 rev
		block_id TEXT NOT NULL,
		doc_id  TEXT NOT NULL,
		op      TEXT NOT NULL,                  -- insert / update / delete
		markdown TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '',
		device_id INTEGER NOT NULL,
		ts INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	// 最新块状态表：每个块只保留最新一条（用于全量同步与快照）
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS blocks(
		block_id TEXT PRIMARY KEY,
		doc_id  TEXT NOT NULL,
		op      TEXT NOT NULL,
		markdown TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '',
		rev INTEGER NOT NULL,                   -- 对应 changes.id
		device_id INTEGER NOT NULL,
		updated INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	// 冲突表：两个设备改了同一块时记录两个版本，等用户在插件里裁决
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS conflicts(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		block_id TEXT NOT NULL,
		doc_id TEXT NOT NULL,
		version_a INTEGER NOT NULL,             -- 服务端当前版本（changes.id）
		version_b INTEGER NOT NULL,             -- 挑战版本（changes.id）
		status TEXT NOT NULL DEFAULT 'open',    -- open / resolved
		chosen INTEGER NOT NULL DEFAULT 0,      -- 胜出版本（changes.id）
		created INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	// 快照表：快照 = 创建时刻 blocks 表的完整拷贝，支持手动回滚
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS snapshots(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		created INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS snapshot_blocks(
		snapshot_id INTEGER NOT NULL,
		block_id TEXT NOT NULL,
		doc_id  TEXT NOT NULL,
		op      TEXT NOT NULL,
		markdown TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

// genKey 生成 32 字节十六进制随机密钥（设备密钥）
func genKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// createDevice 注册新设备，返回密钥
func createDevice(name string) (int64, string, error) {
	key := genKey()
	res, err := db.Exec("INSERT INTO devices(name,key,created,last_seen) VALUES(?,?,?,?)",
		name, key, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	return id, key, nil
}

// deviceByKey 根据设备密钥查设备（鉴权用）
func deviceByKey(key string) (int64, string, error) {
	var id int64
	var name string
	err := db.QueryRow("SELECT id,name FROM devices WHERE key=?", key).Scan(&id, &name)
	return id, name, err
}

// touchDevice 更新设备在线时间
func touchDevice(id int64) {
	db.Exec("UPDATE devices SET last_seen=? WHERE id=?", time.Now().Unix(), id)
}

// BlockChange 一次块变更（API 层与存储层共用的结构体）
type BlockChange struct {
	BlockID  string `json:"blockId"`
	DocID    string `json:"docId"`
	Op       string `json:"op"` // insert / update / delete
	Markdown string `json:"markdown"`
	ParentID string `json:"parentId,omitempty"`
	BaseRev  int64  `json:"baseRev"` // 客户端本地已知的该块版本号（用于冲突检测）
}

// insertChange 写入变更日志；返回变更 id（即 rev）
func insertChange(c *BlockChange, deviceID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO changes(block_id,doc_id,op,markdown,parent_id,device_id,ts)
		VALUES(?,?,?,?,?,?,?)`,
		c.BlockID, c.DocID, c.Op, c.Markdown, c.ParentID, deviceID, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// applyChange 把变更写入最新块状态表（无冲突时调用）
func applyChange(c *BlockChange, deviceID int64, rev int64) {
	if c.Op == "delete" {
		db.Exec("DELETE FROM blocks WHERE block_id=?", c.BlockID)
		return
	}
	_, err := db.Exec(`INSERT INTO blocks(block_id,doc_id,op,markdown,parent_id,rev,device_id,updated)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(block_id) DO UPDATE SET
		doc_id=excluded.doc_id, op=excluded.op, markdown=excluded.markdown,
		parent_id=excluded.parent_id, rev=excluded.rev, device_id=excluded.device_id, updated=excluded.updated`,
		c.BlockID, c.DocID, c.Op, c.Markdown, c.ParentID, rev, deviceID, time.Now().Unix())
	if err != nil {
		log.Printf("[db] 写入 blocks 失败: %v", err)
	}
}

// createConflict 记录一次块冲突（保存两个版本，由用户裁决）
func createConflict(blockID, docID string, verA, verB int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO conflicts(block_id,doc_id,version_a,version_b,created)
		VALUES(?,?,?,?,?)`, blockID, docID, verA, verB, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// getChangeByID 读取变更日志中的某一条（冲突版本详情用）
func getChangeByID(id int64) (*BlockChange, error) {
	c := &BlockChange{}
	var devID int64
	var ts int64
	err := db.QueryRow(`SELECT block_id,doc_id,op,markdown,parent_id,device_id,ts
		FROM changes WHERE id=?`, id).Scan(&c.BlockID, &c.DocID, &c.Op, &c.Markdown, &c.ParentID, &devID, &ts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// createSnapshot 把当前 blocks 全量拷贝进快照
func createSnapshot(name string) (int64, error) {
	if name == "" {
		name = time.Now().Format("2006-01-02 15:04:05")
	}
	res, err := db.Exec("INSERT INTO snapshots(name,created) VALUES(?,?)", name, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	sid, _ := res.LastInsertId()
	_, err = db.Exec(`INSERT INTO snapshot_blocks(snapshot_id,block_id,doc_id,op,markdown,parent_id)
		SELECT ?,block_id,doc_id,op,markdown,parent_id FROM blocks`, sid)
	return sid, err
}

// listSnapshots 快照列表
type SnapshotInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
	Blocks  int64  `json:"blocks"`
}

func listSnapshots() ([]SnapshotInfo, error) {
	rows, err := db.Query(`SELECT s.id,s.name,s.created,
		(SELECT COUNT(*) FROM snapshot_blocks b WHERE b.snapshot_id=s.id) AS cnt
		FROM snapshots s ORDER BY s.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotInfo
	for rows.Next() {
		var s SnapshotInfo
		rows.Scan(&s.ID, &s.Name, &s.Created, &s.Blocks)
		out = append(out, s)
	}
	return out, nil
}

// FullBlock 全量同步返回的单块状态
type FullBlock struct {
	BlockID  string `json:"blockId"`
	DocID    string `json:"docId"`
	Op       string `json:"op"`
	Markdown string `json:"markdown"`
	ParentID string `json:"parentId,omitempty"`
	Rev      int64  `json:"rev"`
}

// getFullBlocks 读取最新块状态（全量同步 / 快照回滚数据源）
func getFullBlocks(snapshotID int64) ([]FullBlock, error) {
	var rows *sql.Rows
	var err error
	if snapshotID > 0 {
		rows, err = db.Query(`SELECT block_id,doc_id,op,markdown,parent_id,0 FROM snapshot_blocks WHERE snapshot_id=?`, snapshotID)
	} else {
		rows, err = db.Query(`SELECT block_id,doc_id,op,markdown,parent_id,rev FROM blocks`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FullBlock
	for rows.Next() {
		var b FullBlock
		rows.Scan(&b.BlockID, &b.DocID, &b.Op, &b.Markdown, &b.ParentID, &b.Rev)
		out = append(out, b)
	}
	return out, nil
}
