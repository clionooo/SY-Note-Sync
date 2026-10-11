// SY Note Sync 服务端 - 管理后台接口
//
// 鉴权：请求头 X-Admin-Token 或 URL 参数 ?token=
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// authAdmin 中间件
func authAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Admin-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token != cfgAdminToken {
			fail(w, 401, "管理令牌错误")
			return
		}
		next(w, r)
	}
}

// decode 读 JSON body（空 body 不算错）
func decode(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "EOF") {
		return nil
	}
	return err
}

// ---------------- 看板总览 ----------------

// GET /api/admin/data
// 一次把后台首页要的东西都给出去，减少前端请求次数
func handleAdminData(w http.ResponseWriter, r *http.Request) {
	devices, _ := listDevices()
	online := hubRef.onlineIDs()
	for i := range devices {
		devices[i].Online = online[devices[i].ID]
		devices[i].Key = "" // 列表不返回密钥，要看单独调 /devices/key
		db.QueryRow(`SELECT COUNT(*) FROM changes WHERE device_id=?`, devices[i].ID).Scan(&devices[i].ChangeCount)
		db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE device_id=? AND deleted=0`, devices[i].ID).Scan(&devices[i].BlockCount)
	}
	logs, total, _ := queryChanges(ChangeFilter{Page: 1, Size: 20})
	conflicts, _ := listConflicts(true)
	snapshots, _ := listSnapshots()
	shares, _ := listShares()
	notebooks, _ := listNotebooks()

	ok(w, map[string]interface{}{
		"stats":     stats(cfgDBPath),
		"devices":   devices,
		"logs":      logs,
		"logTotal":  total,
		"conflicts": conflicts,
		"snapshots": snapshots,
		"shares":    shares,
		"notebooks": notebooks,
		"settings":  allSettings(),
		"version":   ServerVersion,
		"protocol":  ProtocolVersion,
		"now":       now(),
	})
}

// ---------------- 设备管理 ----------------

func handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := listDevices()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	online := hubRef.onlineIDs()
	for i := range devices {
		devices[i].Online = online[devices[i].ID]
		devices[i].Key = ""
	}
	ok(w, map[string]interface{}{"devices": devices})
}

// POST /api/admin/device/update {id, name, note}
func handleAdminDeviceUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := decode(r, &in); err != nil || in.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	if err := updateDevice(in.ID, in.Name, in.Note); err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"ok": true})
}

// POST /api/admin/device/delete {id}
func handleAdminDeviceDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil || in.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	if err := deleteDevice(in.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("[admin] 删除设备 %d", in.ID)
	ok(w, map[string]interface{}{"ok": true})
}

// POST /api/admin/device/rotate {id}  重新发一把设备密钥
func handleAdminDeviceRotate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil || in.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	key, err := rotateDeviceKey(in.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("[admin] 重置设备 %d 的密钥", in.ID)
	ok(w, map[string]interface{}{"ok": true, "key": key})
}

// GET /api/admin/device/key?id=1
// 「查看设备密钥」——手机端注册失败时可以把密钥抄回去手填
func handleAdminDeviceKey(w http.ResponseWriter, r *http.Request) {
	id := atoiDefault(r.URL.Query().Get("id"), 0)
	d, err := getDevice(id)
	if err != nil {
		fail(w, 404, "设备不存在")
		return
	}
	ok(w, map[string]interface{}{"id": d.ID, "name": d.Name, "key": d.Key})
}

// ---------------- 变更日志 ----------------

// GET /api/admin/logs?page=1&size=50&device=&op=&q=&blockId=
func handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ChangeFilter{
		DeviceID: atoiDefault(q.Get("device"), 0),
		Op:       q.Get("op"),
		Keyword:  q.Get("q"),
		BlockID:  q.Get("blockId"),
		Page:     int(atoiDefault(q.Get("page"), 1)),
		Size:     int(atoiDefault(q.Get("size"), 50)),
	}
	logs, total, err := queryChanges(f)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"logs": logs, "total": total, "page": f.Page, "size": f.Size})
}

// GET /api/admin/block/history?blockId=xxx
func handleAdminBlockHistory(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("blockId")
	if id == "" {
		fail(w, 400, "缺少 blockId")
		return
	}
	list, err := blockHistory(id, 100)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	block, exists, _ := getBlock(id)
	ok(w, map[string]interface{}{"history": list, "block": block, "exists": exists})
}

// POST /api/admin/block/restore {changeId}
func handleAdminBlockRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChangeID int64 `json:"changeId"`
	}
	if err := decode(r, &in); err != nil || in.ChangeID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	rc, err := getChange(in.ChangeID)
	if err != nil {
		fail(w, 404, "变更记录不存在")
		return
	}
	op := rc.Op
	if op == "delete" {
		op = "insert"
	}
	block := BlockChange{BlockID: rc.BlockID, DocID: rc.DocID, NotebookID: rc.NotebookID, Op: op, Content: rc.Content}
	rev, err := appendChange(0, block, now())
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	block.Rev = rev
	cur, _, _ := getBlock(rc.BlockID)
	block.ParentID = cur.ParentID
	if err := upsertBlock(0, block, rev, now()); err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("[restore] Web后台恢复块 %s 到版本 %d (op=%s)", rc.BlockID, in.ChangeID, op)
	hubRef.broadcastToDevices(0, map[string]interface{}{"type": "remote_change", "data": block})
	ok(w, map[string]interface{}{"ok": true, "block": block})
}

// GET /api/admin/trash
func handleAdminTrash(w http.ResponseWriter, r *http.Request) {
	list, err := trash()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"trash": list})
}

// ---------------- 冲突 ----------------

func handleAdminConflicts(w http.ResponseWriter, r *http.Request) {
	list, err := listConflicts(r.URL.Query().Get("all") != "1")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"conflicts": list})
}

// POST /api/admin/conflict/resolve {conflictId, chosenChangeId}
func handleAdminConflictResolve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConflictID   int64 `json:"conflictId"`
		ChosenChange int64 `json:"chosenChangeId"`
	}
	if err := decode(r, &in); err != nil || in.ConflictID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	c, err := getConflict(in.ConflictID)
	if err != nil {
		fail(w, 404, "冲突不存在")
		return
	}
	chosen := c.VersionA
	if in.ChosenChange != 0 {
		chosen = c.VersionB
	}
	rev := latestRev() + 1
	if err := upsertBlock(0, chosen, rev, now()); err != nil {
		fail(w, 500, err.Error())
		return
	}
	_ = markConflictResolved(in.ConflictID, chosen.Rev)
	log.Printf("[conflict] Web后台解决冲突 #%d，块 %s 采用版本 %d", in.ConflictID, c.BlockID, chosen.Rev)
	hubRef.broadcastToDevices(0, map[string]interface{}{"type": "remote_change", "data": chosen})
	ok(w, map[string]interface{}{"ok": true})
}

// ---------------- 快照 ----------------

func handleAdminSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := listSnapshots()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"snapshots": list})
}

// POST /api/admin/snapshot/create {name, note}
func handleAdminSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	_ = decode(r, &in)
	if in.Name == "" {
		in.Name = "手动快照"
	}
	id, err := createSnapshot(in.Name, false, in.Note)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("[snapshot] 管理后台创建快照 #%d (%s)", id, in.Name)
	ok(w, map[string]interface{}{"id": id})
}

// POST /api/admin/snapshot/delete {id}
func handleAdminSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil || in.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	if err := deleteSnapshot(in.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"ok": true})
}

// POST /api/admin/snapshot/rollback {id}
// 把服务端的块状态整体还原成快照时刻的样子，然后通知所有设备做全量同步
func handleAdminSnapshotRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil || in.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	blocks, err := snapshotBlocks(in.ID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if len(blocks) == 0 {
		fail(w, 400, "该快照没有内容")
		return
	}
	// 先清空当前块状态，再按快照写回
	if _, err := db.Exec(`DELETE FROM blocks`); err != nil {
		fail(w, 500, err.Error())
		return
	}
	rev := latestRev()
	for _, b := range blocks {
		rev++
		_ = upsertBlock(0, b, rev, now())
	}
	log.Printf("[snapshot] 管理后台回滚到快照 #%d，共 %d 个块", in.ID, len(blocks))
	// 通知所有在线设备重新全量同步
	hubRef.broadcastToDevices(0, map[string]interface{}{"type": "reload", "reason": "snapshot_rollback"})
	ok(w, map[string]interface{}{"ok": true, "blocks": len(blocks)})
}

// ---------------- 笔记本 / 文档 ----------------

func handleAdminNotebooks(w http.ResponseWriter, r *http.Request) {
	list, err := listNotebooks()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"notebooks": list})
}

func handleAdminDocs(w http.ResponseWriter, r *http.Request) {
	list, err := listDocs(r.URL.Query().Get("notebook"))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"docs": list})
}

// GET /api/admin/doc/blocks?doc=xxx  看某个文档里有哪些块
func handleAdminDocBlocks(w http.ResponseWriter, r *http.Request) {
	docID := r.URL.Query().Get("doc")
	if docID == "" {
		fail(w, 400, "缺少 doc")
		return
	}
	rows, err := db.Query(`SELECT block_id,doc_id,notebook_id,op,content,parent_id,rev,device_id,updated,deleted
		FROM blocks WHERE doc_id=? ORDER BY rev ASC LIMIT 2000`, docID)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []BlockState{}
	for rows.Next() {
		var b BlockState
		var del int
		if err := rows.Scan(&b.BlockID, &b.DocID, &b.NotebookID, &b.Op, &b.Content,
			&b.ParentID, &b.Rev, &b.DeviceID, &b.Updated, &del); err != nil {
			continue
		}
		b.Deleted = del == 1
		out = append(out, b)
	}
	ok(w, map[string]interface{}{"blocks": out})
}

// ---------------- 分享 ----------------

func handleAdminShares(w http.ResponseWriter, r *http.Request) {
	list, err := listShares()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]interface{}{"shares": list})
}

// POST /api/admin/share/create {title, blockId, docId, password, expiresDays}
func handleAdminShareCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title       string `json:"title"`
		BlockID     string `json:"blockId"`
		DocID       string `json:"docId"`
		Password    string `json:"password"`
		ExpiresDays int    `json:"expiresDays"`
	}
	_ = decode(r, &in)
	if in.BlockID == "" && in.DocID == "" {
		fail(w, 400, "至少指定一个块或文档")
		return
	}
	if getSetting("share_enabled", "1") != "1" {
		fail(w, 403, "服务端已关闭分享功能")
		return
	}
	var expires int64
	if in.ExpiresDays > 0 {
		expires = now() + int64(in.ExpiresDays)*86400
	}
	s, err := createShare(in.Title, in.BlockID, in.DocID, in.Password, expires)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("[share] 创建分享 %s (%s)", s.Token, in.Title)
	ok(w, map[string]interface{}{"ok": true, "share": s})
}

// POST /api/admin/share/delete {id}
func handleAdminShareDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil || in.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	_ = deleteShare(in.ID)
	ok(w, map[string]interface{}{"ok": true})
}

// ---------------- 设置 ----------------

func handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		ok(w, map[string]interface{}{"settings": allSettings(), "adminTokenMasked": maskToken(cfgAdminToken)})
		return
	}
	var in map[string]string
	if err := decode(r, &in); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	for k, v := range in {
		if err := setSetting(k, v); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	log.Printf("[admin] 更新设置 %d 项", len(in))
	ok(w, map[string]interface{}{"ok": true, "settings": allSettings()})
}

// maskToken 只露出前后各 4 位，避免后台页面明文显示管理令牌
func maskToken(t string) string {
	if len(t) <= 8 {
		return strings.Repeat("*", len(t))
	}
	return t[:4] + strings.Repeat("*", len(t)-8) + t[len(t)-4:]
}

// ---------------- 版本信息（公开） ----------------

func handleVersion(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]interface{}{
		"server":       ServerVersion,
		"protocol":     ProtocolVersion,
		"minPlugin":    MinPluginVersion,
		"serverName":   getSetting("site_name", "SY Note Sync"),
		"shareEnabled": getSetting("share_enabled", "1") == "1",
		"registerOpen": getSetting("allow_register", "1") == "1",
	})
}

// ---------------- 公开分享页 ----------------

// GET /api/share/{token}?password=xxx
func handleShareAPI(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/api/share/")
	s, found, err := getShareByToken(token)
	if err != nil || !found {
		fail(w, 404, "分享不存在")
		return
	}
	if s.Expires > 0 && now() > s.Expires {
		fail(w, 410, "分享已过期")
		return
	}
	if s.Password != "" && r.URL.Query().Get("password") != s.Password {
		writeJSON(w, 401, map[string]interface{}{"error": "需要密码", "needPassword": true, "title": s.Title})
		return
	}
	incShareViews(s.ID)

	type item struct {
		BlockID string `json:"blockId"`
		DocID   string `json:"docId"`
		Op      string `json:"op"`
		Content string `json:"content"`
		Updated int64  `json:"updated"`
	}
	items := []item{}
	if s.BlockID != "" {
		if b, exists, _ := getBlock(s.BlockID); exists {
			items = append(items, item{b.BlockID, b.DocID, b.Op, b.Content, b.Updated})
		}
	}
	if s.DocID != "" {
		rows, err := db.Query(`SELECT block_id,doc_id,op,content,updated FROM blocks
			WHERE doc_id=? AND deleted=0 ORDER BY rev ASC LIMIT 1000`, s.DocID)
		if err == nil {
			for rows.Next() {
				var it item
				if rows.Scan(&it.BlockID, &it.DocID, &it.Op, &it.Content, &it.Updated) == nil {
					items = append(items, it)
				}
			}
			rows.Close()
		}
	}
	ok(w, map[string]interface{}{
		"title": s.Title, "created": s.Created, "views": s.Views + 1, "items": items,
	})
}

// GET /share/{token}  分享页 HTML（前端拿到 token 后自己调 /api/share/{token}）
func handleSharePage(w http.ResponseWriter, r *http.Request) {
	serveEmbedded(w, r, "web/share.html")
}

// ---------------- 后台页面 ----------------

func handleAdminPage(w http.ResponseWriter, r *http.Request) {
	serveEmbedded(w, r, "web/admin.html")
}

// GET /api/admin/export  导出全部块状态（JSON 备份）
func handleAdminExport(w http.ResponseWriter, r *http.Request) {
	blocks, _ := allBlocks(true)
	devices, _ := listDevices()
	for i := range devices {
		devices[i].Key = ""
	}
	w.Header().Set("Content-Disposition",
		"attachment; filename=\"sy-note-sync-backup-"+strconv.FormatInt(now(), 10)+".json\"")
	writeJSON(w, 200, map[string]interface{}{
		"version":   ServerVersion,
		"exported":  now(),
		"blocks":    blocks,
		"devices":   devices,
		"snapshots": func() []Snapshot { s, _ := listSnapshots(); return s }(),
	})
}
