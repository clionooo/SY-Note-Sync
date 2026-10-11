// SY Note Sync 服务端 - SQLite 存储层
//
// 设计要点：
//  1. 所有 SQL 都集中在这个文件，handler 不直接碰 db，方便审查与替换数据库
//  2. changes 表是「流水」——只追加不修改，用于增量拉取与历史回溯
//  3. blocks 表是「最新状态」——增量拉取之外的全量同步、分享页都读它
//  4. 启动时自动建表 + 补齐列（老版本库可以直接升级，不用重建）
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var db *sql.DB

func now() int64 { return time.Now().Unix() }

// randHex 生成 n 字节随机数的十六进制串，用于设备密钥/分享 token
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退回时间戳，保证不 panic
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// initDB 打开数据库、建表、补齐缺失列
func initDB(path string) error {
	var err error
	// _pragma 让 SQLite 开启 WAL，读写并发更稳；busy_timeout 避免瞬时锁冲突直接报错
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // modernc sqlite 单写者，串行化最省心
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS devices(
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			name      TEXT NOT NULL,
			key       TEXT NOT NULL UNIQUE,
			platform  TEXT NOT NULL DEFAULT '',
			version   TEXT NOT NULL DEFAULT '',
			note      TEXT NOT NULL DEFAULT '',
			created   INTEGER NOT NULL,
			last_seen INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS changes(
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id   INTEGER NOT NULL,
			block_id    TEXT NOT NULL,
			doc_id      TEXT NOT NULL DEFAULT '',
			notebook_id TEXT NOT NULL DEFAULT '',
			op          TEXT NOT NULL,
			content     TEXT NOT NULL DEFAULT '',
			parent_id   TEXT NOT NULL DEFAULT '',
			ts          INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_changes_block ON changes(block_id)`,
		`CREATE INDEX IF NOT EXISTS idx_changes_device ON changes(device_id)`,
		`CREATE TABLE IF NOT EXISTS blocks(
			block_id    TEXT PRIMARY KEY,
			doc_id      TEXT NOT NULL DEFAULT '',
			notebook_id TEXT NOT NULL DEFAULT '',
			op          TEXT NOT NULL DEFAULT 'update',
			content     TEXT NOT NULL DEFAULT '',
			parent_id   TEXT NOT NULL DEFAULT '',
			rev         INTEGER NOT NULL DEFAULT 0,
			device_id   INTEGER NOT NULL DEFAULT 0,
			updated     INTEGER NOT NULL DEFAULT 0,
			deleted     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_blocks_doc ON blocks(doc_id)`,
		`CREATE TABLE IF NOT EXISTS conflicts(
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			block_id   TEXT NOT NULL,
			doc_id     TEXT NOT NULL DEFAULT '',
			a_content  TEXT NOT NULL DEFAULT '',
			a_device   INTEGER NOT NULL DEFAULT 0,
			a_rev      INTEGER NOT NULL DEFAULT 0,
			b_content  TEXT NOT NULL DEFAULT '',
			b_device   INTEGER NOT NULL DEFAULT 0,
			b_rev      INTEGER NOT NULL DEFAULT 0,
			created    INTEGER NOT NULL,
			resolved   INTEGER NOT NULL DEFAULT 0,
			chosen_rev INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots(
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			name    TEXT NOT NULL,
			blocks  INTEGER NOT NULL DEFAULT 0,
			created INTEGER NOT NULL,
			note    TEXT NOT NULL DEFAULT '',
			auto    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS snapshot_blocks(
			snapshot_id INTEGER NOT NULL,
			block_id    TEXT NOT NULL,
			doc_id      TEXT NOT NULL DEFAULT '',
			notebook_id TEXT NOT NULL DEFAULT '',
			content     TEXT NOT NULL DEFAULT '',
			parent_id   TEXT NOT NULL DEFAULT '',
			deleted     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_snapblocks ON snapshot_blocks(snapshot_id)`,
		`CREATE TABLE IF NOT EXISTS shares(
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			token    TEXT NOT NULL UNIQUE,
			title    TEXT NOT NULL DEFAULT '',
			block_id TEXT NOT NULL DEFAULT '',
			doc_id   TEXT NOT NULL DEFAULT '',
			password TEXT NOT NULL DEFAULT '',
			views    INTEGER NOT NULL DEFAULT 0,
			created  INTEGER NOT NULL,
			expires  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS notebooks(
			id      TEXT PRIMARY KEY,
			name    TEXT NOT NULL DEFAULT '',
			icon    TEXT NOT NULL DEFAULT '',
			updated INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS docs(
			id          TEXT PRIMARY KEY,
			notebook_id TEXT NOT NULL DEFAULT '',
			title       TEXT NOT NULL DEFAULT '',
			updated     INTEGER NOT NULL DEFAULT 0,
			deleted     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_docs_nb ON docs(notebook_id)`,
		`CREATE TABLE IF NOT EXISTS settings(
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS assets(
			path      TEXT PRIMARY KEY,
			size      INTEGER NOT NULL DEFAULT 0,
			hash      TEXT NOT NULL DEFAULT '',
			mime      TEXT NOT NULL DEFAULT '',
			device_id INTEGER NOT NULL DEFAULT 0,
			updated   INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("建表失败: %w\nSQL: %s", err, s)
		}
	}

	// ---- 老库升级（v1 -> v2）----
	migrateLegacySchema()

	// ⚠️ 顺序很重要：依赖「补列」的索引必须放在迁移之后建，
	// 否则老库（v1 没有 notebook_id 列）会在建索引时直接报 no such column 起不来。
	lateIndexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_blocks_nb ON blocks(notebook_id)`,
	}
	for _, s := range lateIndexes {
		if _, err := db.Exec(s); err != nil {
			log.Printf("[store] 建索引失败（可忽略）: %v", err)
		}
	}

	log.Printf("[store] 数据库就绪: %s", path)
	return nil
}

// hasColumn 判断表里有没有某列
func hasColumn(table, col string) bool {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == col {
			return true
		}
	}
	return false
}

// renameColumn 把 old 列的数据搬到 new 列（old 列保留不删，SQLite 删列麻烦且没收益）
func renameColumn(table, old, new string) {
	if !hasColumn(table, old) || hasColumn(table, new) {
		return
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s TEXT NOT NULL DEFAULT ''", table, new)); err != nil {
		log.Printf("[store] 迁移 %s.%s 失败: %v", table, new, err)
		return
	}
	if _, err := db.Exec(fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s <> ''", table, new, old, old)); err != nil {
		log.Printf("[store] 迁移 %s.%s 数据失败: %v", table, new, err)
		return
	}
	log.Printf("[store] 已迁移 %s.%s -> %s", table, old, new)
}

// migrateLegacySchema 处理 v1 -> v2 的结构差异
//
// v1 的差异：
//   - changes / blocks / snapshot_blocks 里内容列叫 markdown（v2 改叫 content）
//   - blocks 没有 deleted / notebook_id 列
//   - conflicts / snapshots 结构完全不同（v1 是 version_a/version_b 指针，v2 直接存两份内容）
//
// 原则：有数据的表改名保留；结构差太多且 v1 里基本是空的表直接重建。
func migrateLegacySchema() {
	renameColumn("changes", "markdown", "content")
	renameColumn("blocks", "markdown", "content")
	renameColumn("snapshot_blocks", "markdown", "content")

	// conflicts：v1 用 version_a/version_b 指针，v2 直接存两份内容 —— 重建
	if hasColumn("conflicts", "version_a") {
		log.Printf("[store] 检测到 v1 conflicts 表结构，重建（v1 的冲突记录无法直接映射）")
		db.Exec(`DROP TABLE IF EXISTS conflicts`)
		db.Exec(`CREATE TABLE conflicts(
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			block_id   TEXT NOT NULL,
			doc_id     TEXT NOT NULL DEFAULT '',
			a_content  TEXT NOT NULL DEFAULT '',
			a_device   INTEGER NOT NULL DEFAULT 0,
			a_rev      INTEGER NOT NULL DEFAULT 0,
			b_content  TEXT NOT NULL DEFAULT '',
			b_device   INTEGER NOT NULL DEFAULT 0,
			b_rev      INTEGER NOT NULL DEFAULT 0,
			created    INTEGER NOT NULL,
			resolved   INTEGER NOT NULL DEFAULT 0,
			chosen_rev INTEGER NOT NULL DEFAULT 0
		)`)
	}

	// 常规补列（幂等）
	ensureColumn("changes", "notebook_id", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("changes", "content", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("blocks", "notebook_id", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("blocks", "content", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("blocks", "deleted", "INTEGER NOT NULL DEFAULT 0")
	ensureColumn("devices", "platform", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("devices", "version", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("devices", "note", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("snapshots", "note", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("snapshots", "auto", "INTEGER NOT NULL DEFAULT 0")
	ensureColumn("snapshots", "blocks", "INTEGER NOT NULL DEFAULT 0")
	ensureColumn("conflicts", "a_content", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("conflicts", "b_content", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("conflicts", "resolved", "INTEGER NOT NULL DEFAULT 0")
	ensureColumn("conflicts", "chosen_rev", "INTEGER NOT NULL DEFAULT 0")
	ensureColumn("snapshot_blocks", "content", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("snapshot_blocks", "notebook_id", "TEXT NOT NULL DEFAULT ''")
	ensureColumn("snapshot_blocks", "deleted", "INTEGER NOT NULL DEFAULT 0")

	// 被删的块标记一下，回收站/全量同步才准
	db.Exec(`UPDATE blocks SET deleted=1 WHERE op='delete' AND deleted=0`)
}

// ensureColumn 给已有表补列，已存在则忽略
func ensureColumn(table, col, decl string) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil && name == col {
			exists = true
		}
	}
	if exists {
		return
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl)); err != nil {
		log.Printf("[store] 补列 %s.%s 失败: %v", table, col, err)
	}
}

// ==================== 设备 ====================

func createDevice(name string) (int64, string, error) {
	key := randHex(16)
	res, err := db.Exec(`INSERT INTO devices(name,key,created,last_seen) VALUES(?,?,?,?)`,
		name, key, now(), now())
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	return id, key, nil
}

func deviceByKey(key string) (Device, error) {
	var d Device
	err := db.QueryRow(`SELECT id,name,platform,version,note,created,last_seen FROM devices WHERE key=?`, key).
		Scan(&d.ID, &d.Name, &d.Platform, &d.Version, &d.Note, &d.Created, &d.LastSeen)
	d.Key = key
	return d, err
}

func touchDevice(id int64) {
	db.Exec(`UPDATE devices SET last_seen=? WHERE id=?`, now(), id)
}

func setDeviceInfo(id int64, platform, version string) {
	if platform == "" && version == "" {
		return
	}
	db.Exec(`UPDATE devices SET platform=CASE WHEN ?<>'' THEN ? ELSE platform END,
	         version=CASE WHEN ?<>'' THEN ? ELSE version END WHERE id=?`,
		platform, platform, version, version, id)
}

func listDevices() ([]Device, error) {
	rows, err := db.Query(`SELECT id,name,key,platform,version,note,created,last_seen FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.Key, &d.Platform, &d.Version, &d.Note, &d.Created, &d.LastSeen); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func getDevice(id int64) (Device, error) {
	var d Device
	err := db.QueryRow(`SELECT id,name,key,platform,version,note,created,last_seen FROM devices WHERE id=?`, id).
		Scan(&d.ID, &d.Name, &d.Key, &d.Platform, &d.Version, &d.Note, &d.Created, &d.LastSeen)
	return d, err
}

// updateDevice 只更新传了值的字段（空字符串表示不改）
func updateDevice(id int64, name, note string) error {
	if name != "" {
		if _, err := db.Exec(`UPDATE devices SET name=? WHERE id=?`, name, id); err != nil {
			return err
		}
	}
	if note != "" {
		if _, err := db.Exec(`UPDATE devices SET note=? WHERE id=?`, note, id); err != nil {
			return err
		}
	}
	return nil
}

func deleteDevice(id int64) error {
	_, err := db.Exec(`DELETE FROM devices WHERE id=?`, id)
	return err
}

// rotateDeviceKey 重新发一把设备密钥（原密钥立即失效）
func rotateDeviceKey(id int64) (string, error) {
	key := randHex(16)
	_, err := db.Exec(`UPDATE devices SET key=? WHERE id=?`, key, id)
	return key, err
}

// ==================== 变更流水 ====================

func appendChange(deviceID int64, c BlockChange, ts int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO changes(device_id,block_id,doc_id,notebook_id,op,content,parent_id,ts)
		VALUES(?,?,?,?,?,?,?,?)`,
		deviceID, c.BlockID, c.DocID, c.NotebookID, c.Op, c.Content, c.ParentID, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// changesSince 增量拉取：id 大于 since 且不是本设备产生的
func changesSince(since, excludeDevice int64, limit int) ([]BlockChange, int64, error) {
	if limit <= 0 || limit > MaxPullLimit {
		limit = MaxPullLimit
	}
	rows, err := db.Query(`SELECT id,block_id,doc_id,notebook_id,op,content,parent_id
		FROM changes WHERE id>? AND device_id<>? ORDER BY id ASC LIMIT ?`, since, excludeDevice, limit)
	if err != nil {
		return nil, since, err
	}
	defer rows.Close()
	out := []BlockChange{}
	latest := since
	for rows.Next() {
		var rev int64
		var c BlockChange
		if err := rows.Scan(&rev, &c.BlockID, &c.DocID, &c.NotebookID, &c.Op, &c.Content, &c.ParentID); err != nil {
			continue
		}
		c.Rev = rev
		out = append(out, c)
		latest = rev
	}
	if max := latestRev(); max > latest {
		latest = max
	}
	return out, latest, nil
}

func latestRev() int64 {
	var v int64
	db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM changes`).Scan(&v)
	return v
}

// ChangeFilter 管理后台的日志筛选条件
type ChangeFilter struct {
	DeviceID int64
	Op       string
	Keyword  string
	BlockID  string
	Page     int
	Size     int
}

func queryChanges(f ChangeFilter) ([]LogEntry, int64, error) {
	if f.Size <= 0 || f.Size > MaxLogLimit {
		f.Size = 50
	}
	if f.Page < 1 {
		f.Page = 1
	}
	where := []string{"1=1"}
	args := []interface{}{}
	if f.DeviceID > 0 {
		where = append(where, "c.device_id=?")
		args = append(args, f.DeviceID)
	}
	if f.Op != "" {
		where = append(where, "c.op=?")
		args = append(args, f.Op)
	}
	if f.BlockID != "" {
		where = append(where, "c.block_id=?")
		args = append(args, f.BlockID)
	}
	if f.Keyword != "" {
		where = append(where, "(c.block_id LIKE ? OR c.doc_id LIKE ? OR c.content LIKE ?)")
		kw := "%" + f.Keyword + "%"
		args = append(args, kw, kw, kw)
	}
	cond := strings.Join(where, " AND ")

	var total int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM changes c WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sqlArgs := append(append([]interface{}{}, args...), f.Size, (f.Page-1)*f.Size)
	rows, err := db.Query(`SELECT c.id,c.block_id,c.doc_id,c.notebook_id,c.op,c.device_id,
		COALESCE(d.name,''),c.ts,c.content
		FROM changes c LEFT JOIN devices d ON d.id=c.device_id
		WHERE `+cond+` ORDER BY c.id DESC LIMIT ? OFFSET ?`, sqlArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.BlockID, &e.DocID, &e.NotebookID, &e.Op,
			&e.DeviceID, &e.DeviceName, &e.Ts, &e.Content); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, total, nil
}

// blockHistory 某个块的全部历史版本（最新在前）
func blockHistory(blockID string, limit int) ([]LogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`SELECT c.id,c.block_id,c.doc_id,c.op,c.device_id,
		COALESCE(d.name,''),c.ts,c.content
		FROM changes c LEFT JOIN devices d ON d.id=c.device_id
		WHERE c.block_id=? ORDER BY c.id DESC LIMIT ?`, blockID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.BlockID, &e.DocID, &e.Op, &e.DeviceID, &e.DeviceName, &e.Ts, &e.Content); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// getChange 取单条变更（回收站恢复用）
func getChange(id int64) (LogEntry, error) {
	var e LogEntry
	err := db.QueryRow(`SELECT id,block_id,doc_id,notebook_id,op,device_id,ts,content
		FROM changes WHERE id=?`, id).
		Scan(&e.ID, &e.BlockID, &e.DocID, &e.NotebookID, &e.Op, &e.DeviceID, &e.Ts, &e.Content)
	return e, err
}

// ==================== 块最新状态 ====================

func upsertBlock(deviceID int64, c BlockChange, rev, ts int64) error {
	deleted := 0
	if c.Op == "delete" {
		deleted = 1
	}
	content := c.Content
	if c.Op == "delete" {
		content = "" // 删除时清空内容，避免回收站里还留着“活”的内容
	}
	_, err := db.Exec(`INSERT INTO blocks(block_id,doc_id,notebook_id,op,content,parent_id,rev,device_id,updated,deleted)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(block_id) DO UPDATE SET
			doc_id=excluded.doc_id,
			notebook_id=CASE WHEN excluded.notebook_id<>'' THEN excluded.notebook_id ELSE blocks.notebook_id END,
			op=excluded.op,
			content=excluded.content,
			parent_id=CASE WHEN excluded.parent_id<>'' THEN excluded.parent_id ELSE blocks.parent_id END,
			rev=excluded.rev,
			device_id=excluded.device_id,
			updated=excluded.updated,
			deleted=excluded.deleted`,
		c.BlockID, c.DocID, c.NotebookID, c.Op, content, c.ParentID, rev, deviceID, ts, deleted)
	return err
}

func getBlock(blockID string) (BlockState, bool, error) {
	var b BlockState
	var deleted int
	err := db.QueryRow(`SELECT block_id,doc_id,notebook_id,op,content,parent_id,rev,device_id,updated,deleted
		FROM blocks WHERE block_id=?`, blockID).
		Scan(&b.BlockID, &b.DocID, &b.NotebookID, &b.Op, &b.Content, &b.ParentID, &b.Rev, &b.DeviceID, &b.Updated, &deleted)
	if err == sql.ErrNoRows {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	b.Deleted = deleted == 1
	return b, true, nil
}

// allBlocks 全量同步用。includeDeleted=false 时只返回活着的块。
func allBlocks(includeDeleted bool) ([]BlockChange, error) {
	q := `SELECT block_id,doc_id,notebook_id,op,content,parent_id,rev FROM blocks`
	if !includeDeleted {
		q += ` WHERE deleted=0`
	}
	q += ` ORDER BY rev ASC`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockChange{}
	for rows.Next() {
		var c BlockChange
		if err := rows.Scan(&c.BlockID, &c.DocID, &c.NotebookID, &c.Op, &c.Content, &c.ParentID, &c.Rev); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// trash 回收站：每个被删的块取最近一条删除记录
func trash() ([]LogEntry, error) {
	rows, err := db.Query(`SELECT c.id,c.block_id,c.doc_id,c.op,c.device_id,COALESCE(d.name,''),c.ts,c.content
		FROM changes c
		JOIN (SELECT block_id, MAX(id) AS mid FROM changes WHERE op='delete' GROUP BY block_id) t
		  ON t.mid = c.id
		LEFT JOIN devices d ON d.id=c.device_id
		ORDER BY c.id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.BlockID, &e.DocID, &e.Op, &e.DeviceID, &e.DeviceName, &e.Ts, &e.Content); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ==================== 冲突 ====================

func createConflict(blockID, docID string, a BlockChange, b BlockChange) (int64, error) {
	res, err := db.Exec(`INSERT INTO conflicts(block_id,doc_id,a_content,a_device,a_rev,b_content,b_device,b_rev,created)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		blockID, docID, a.Content, 0, a.Rev, b.Content, 0, b.Rev, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func listConflicts(onlyOpen bool) ([]Conflict, error) {
	q := `SELECT id,block_id,doc_id,a_content,a_device,a_rev,b_content,b_device,b_rev,created,resolved,chosen_rev
		FROM conflicts`
	if onlyOpen {
		q += ` WHERE resolved=0`
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conflict{}
	for rows.Next() {
		var c Conflict
		var aDev, bDev int64
		var resolved int
		if err := rows.Scan(&c.ID, &c.BlockID, &c.DocID, &c.VersionA.Content, &aDev,
			&c.VersionA.Rev, &c.VersionB.Content, &bDev, &c.VersionB.Rev,
			&c.Created, &resolved, &c.ChosenRev); err != nil {
			continue
		}
		c.Resolved = resolved == 1
		// 把块/文档信息回填到两个版本里，前端就能直接渲染对比
		for _, v := range []*BlockChange{&c.VersionA, &c.VersionB} {
			v.BlockID = c.BlockID
			v.DocID = c.DocID
		}
		c.VersionA.BaseRev = aDev
		c.VersionB.BaseRev = bDev
		out = append(out, c)
	}
	return out, nil
}

func getConflict(id int64) (Conflict, error) {
	var c Conflict
	var resolved int
	err := db.QueryRow(`SELECT id,block_id,doc_id,a_content,a_rev,b_content,b_rev,created,resolved,chosen_rev
		FROM conflicts WHERE id=?`, id).
		Scan(&c.ID, &c.BlockID, &c.DocID, &c.VersionA.Content, &c.VersionA.Rev,
			&c.VersionB.Content, &c.VersionB.Rev, &c.Created, &resolved, &c.ChosenRev)
	c.Resolved = resolved == 1
	return c, err
}

func markConflictResolved(id, chosenRev int64) error {
	_, err := db.Exec(`UPDATE conflicts SET resolved=1, chosen_rev=? WHERE id=?`, chosenRev, id)
	return err
}

// ==================== 快照 ====================

func createSnapshot(name string, auto bool, note string) (int64, error) {
	blocks, err := allBlocks(true)
	if err != nil {
		return 0, err
	}
	a := 0
	if auto {
		a = 1
	}
	res, err := db.Exec(`INSERT INTO snapshots(name,blocks,created,note,auto) VALUES(?,?,?,?,?)`,
		name, len(blocks), now(), note, a)
	if err != nil {
		return 0, err
	}
	sid, _ := res.LastInsertId()
	for _, b := range blocks {
		deleted := 0
		if b.Op == "delete" {
			deleted = 1
		}
		db.Exec(`INSERT INTO snapshot_blocks(snapshot_id,block_id,doc_id,notebook_id,content,parent_id,deleted)
			VALUES(?,?,?,?,?,?,?)`, sid, b.BlockID, b.DocID, b.NotebookID, b.Content, b.ParentID, deleted)
	}
	return sid, nil
}

func listSnapshots() ([]Snapshot, error) {
	rows, err := db.Query(`SELECT id,name,blocks,created,note,auto FROM snapshots ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var s Snapshot
		var a int
		if err := rows.Scan(&s.ID, &s.Name, &s.Blocks, &s.Created, &s.Note, &a); err != nil {
			continue
		}
		s.Auto = a == 1
		out = append(out, s)
	}
	return out, nil
}

func deleteSnapshot(id int64) error {
	db.Exec(`DELETE FROM snapshot_blocks WHERE snapshot_id=?`, id)
	_, err := db.Exec(`DELETE FROM snapshots WHERE id=?`, id)
	return err
}

// snapshotBlocks 回滚用：返回快照时刻的全部块状态
func snapshotBlocks(id int64) ([]BlockChange, error) {
	rows, err := db.Query(`SELECT block_id,doc_id,notebook_id,content,parent_id,deleted
		FROM snapshot_blocks WHERE snapshot_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockChange{}
	for rows.Next() {
		var c BlockChange
		var deleted int
		if err := rows.Scan(&c.BlockID, &c.DocID, &c.NotebookID, &c.Content, &c.ParentID, &deleted); err != nil {
			continue
		}
		c.Op = "update"
		if deleted == 1 {
			c.Op = "delete"
		}
		out = append(out, c)
	}
	return out, nil
}

// pruneSnapshots 只保留最近 keep 个自动快照（手动快照不删）
func pruneSnapshots(keep int) {
	if keep <= 0 {
		return
	}
	rows, err := db.Query(`SELECT id FROM snapshots WHERE auto=1 ORDER BY id DESC LIMIT -1 OFFSET ?`, keep)
	if err != nil {
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		deleteSnapshot(id)
	}
}

// ==================== 分享 ====================

func createShare(title, blockID, docID, password string, expires int64) (Share, error) {
	token := randHex(8)
	res, err := db.Exec(`INSERT INTO shares(token,title,block_id,doc_id,password,created,expires)
		VALUES(?,?,?,?,?,?,?)`, token, title, blockID, docID, password, now(), expires)
	if err != nil {
		return Share{}, err
	}
	id, _ := res.LastInsertId()
	return Share{ID: id, Token: token, Title: title, BlockID: blockID, DocID: docID,
		Password: password, Created: now(), Expires: expires}, nil
}

func listShares() ([]Share, error) {
	rows, err := db.Query(`SELECT id,token,title,block_id,doc_id,password,views,created,expires
		FROM shares ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Share{}
	for rows.Next() {
		var s Share
		if err := rows.Scan(&s.ID, &s.Token, &s.Title, &s.BlockID, &s.DocID, &s.Password,
			&s.Views, &s.Created, &s.Expires); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func getShareByToken(token string) (Share, bool, error) {
	var s Share
	err := db.QueryRow(`SELECT id,token,title,block_id,doc_id,password,views,created,expires
		FROM shares WHERE token=?`, token).
		Scan(&s.ID, &s.Token, &s.Title, &s.BlockID, &s.DocID, &s.Password, &s.Views, &s.Created, &s.Expires)
	if err == sql.ErrNoRows {
		return s, false, nil
	}
	return s, err == nil, err
}

func incShareViews(id int64) {
	db.Exec(`UPDATE shares SET views=views+1 WHERE id=?`, id)
}

func deleteShare(id int64) error {
	_, err := db.Exec(`DELETE FROM shares WHERE id=?`, id)
	return err
}

// ==================== 笔记本 / 文档 ====================

func upsertNotebook(id, name, icon string) error {
	if id == "" {
		return nil
	}
	_, err := db.Exec(`INSERT INTO notebooks(id,name,icon,updated) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE notebooks.name END,
			icon=CASE WHEN excluded.icon<>'' THEN excluded.icon ELSE notebooks.icon END,
			updated=excluded.updated`, id, name, icon, now())
	return err
}

func upsertDoc(id, notebookID, title string, deleted bool) error {
	if id == "" {
		return nil
	}
	d := 0
	if deleted {
		d = 1
	}
	_, err := db.Exec(`INSERT INTO docs(id,notebook_id,title,updated,deleted) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			notebook_id=CASE WHEN excluded.notebook_id<>'' THEN excluded.notebook_id ELSE docs.notebook_id END,
			title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE docs.title END,
			updated=excluded.updated, deleted=excluded.deleted`, id, notebookID, title, now(), d)
	return err
}

func listNotebooks() ([]Notebook, error) {
	rows, err := db.Query(`SELECT n.id,n.name,n.icon,n.updated,
		(SELECT COUNT(*) FROM docs d WHERE d.notebook_id=n.id AND d.deleted=0),
		(SELECT COUNT(*) FROM blocks b WHERE b.notebook_id=n.id AND b.deleted=0)
		FROM notebooks n ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notebook{}
	for rows.Next() {
		var n Notebook
		if err := rows.Scan(&n.ID, &n.Name, &n.Icon, &n.Updated, &n.Docs, &n.Blocks); err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func listDocs(notebookID string) ([]Doc, error) {
	q := `SELECT d.id,d.notebook_id,d.title,d.updated,d.deleted,
		(SELECT COUNT(*) FROM blocks b WHERE b.doc_id=d.id AND b.deleted=0)
		FROM docs d WHERE d.deleted=0`
	args := []interface{}{}
	if notebookID != "" {
		q += ` AND d.notebook_id=?`
		args = append(args, notebookID)
	}
	q += ` ORDER BY d.updated DESC LIMIT 1000`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Doc{}
	for rows.Next() {
		var d Doc
		var del int
		if err := rows.Scan(&d.ID, &d.NotebookID, &d.Title, &d.Updated, &del, &d.Blocks); err != nil {
			continue
		}
		d.Deleted = del == 1
		out = append(out, d)
	}
	return out, nil
}

// ==================== 设置 ====================

func getSetting(key, def string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func setSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func allSettings() map[string]string {
	out := map[string]string{}
	rows, err := db.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// ==================== 附件 ====================

func upsertAsset(a Asset) error {
	_, err := db.Exec(`INSERT INTO assets(path,size,hash,mime,device_id,updated) VALUES(?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			size=excluded.size, hash=excluded.hash, mime=excluded.mime,
			device_id=excluded.device_id, updated=excluded.updated`,
		a.Path, a.Size, a.Hash, a.Mime, a.DeviceID, a.Updated)
	return err
}

func getAsset(path string) (Asset, bool) {
	var a Asset
	err := db.QueryRow(`SELECT path,size,hash,mime,device_id,updated FROM assets WHERE path=?`, path).
		Scan(&a.Path, &a.Size, &a.Hash, &a.Mime, &a.DeviceID, &a.Updated)
	if err != nil {
		return a, false
	}
	return a, true
}

func listAssets(limit int) ([]Asset, error) {
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	rows, err := db.Query(`SELECT path,size,hash,mime,device_id,updated FROM assets
		ORDER BY updated DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		var a Asset
		if rows.Scan(&a.Path, &a.Size, &a.Hash, &a.Mime, &a.DeviceID, &a.Updated) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// missingAssets 返回服务端没有（或哈希对不上）的路径，客户端据此决定要传哪些
func missingAssets(paths []string) []string {
	out := []string{}
	for _, p := range paths {
		if _, ok := getAsset(p); !ok {
			out = append(out, p)
		}
	}
	return out
}

func deleteAsset(path string) error {
	_, err := db.Exec(`DELETE FROM assets WHERE path=?`, path)
	return err
}

func assetStats() (count, bytes int64) {
	db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM assets`).Scan(&count, &bytes)
	return
}

// ==================== 统计 ====================

// osStat 返回文件字节数（数据库文件大小，看板用）
func osStat(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func stats(dbPath string) Stats {
	var s Stats
	db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&s.Devices)
	db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE deleted=0`).Scan(&s.Blocks)
	db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE deleted=1`).Scan(&s.Deleted)
	db.QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&s.Changes)
	db.QueryRow(`SELECT COUNT(*) FROM conflicts WHERE resolved=0`).Scan(&s.Conflicts)
	db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&s.Snapshots)
	db.QueryRow(`SELECT COUNT(*) FROM notebooks`).Scan(&s.Notebooks)
	db.QueryRow(`SELECT COUNT(*) FROM docs WHERE deleted=0`).Scan(&s.Docs)
	db.QueryRow(`SELECT COUNT(*) FROM shares`).Scan(&s.Shares)
	s.Assets, s.AssetBytes = assetStats()
	db.QueryRow(`SELECT COALESCE(MAX(ts),0) FROM changes`).Scan(&s.LastChange)
	if fi, err := osStat(dbPath); err == nil {
		s.DBBytes = fi
	}
	return s
}
