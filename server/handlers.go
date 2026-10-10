// HTTP API 处理器：鉴权、设备注册、同步推送/拉取、快照、冲突、Web 后台
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// ---------- 通用工具 ----------

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// authDevice 中间件：校验请求头 X-Device-Key，把 deviceID 放进 context 变量
// （简化实现：通过自定义 request 封装传递）
type deviceReq struct {
	http.ResponseWriter
	*http.Request
	deviceID int64
}

func authDevice(next func(*deviceReq)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Device-Key")
		if key == "" {
			// WebSocket 也支持 URL 参数传 key（iOS 端部分场景不方便自定义 header）
			key = r.URL.Query().Get("device_key")
		}
		if key == "" {
			fail(w, 401, "缺少设备密钥")
			return
		}
		id, _, err := deviceByKey(key)
		if err != nil {
			fail(w, 401, "设备密钥无效")
			return
		}
		touchDevice(id)
		next(&deviceReq{ResponseWriter: w, Request: r, deviceID: id})
	}
}

// authAdmin 中间件：校验管理令牌（query 或 header）
func authAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			token = r.Header.Get("X-Admin-Token")
		}
		if token != cfgAdminToken {
			fail(w, 401, "管理令牌错误")
			return
		}
		next(w, r)
	}
}

// ---------- 设备注册 ----------
// POST /api/register  {adminToken, deviceName} -> {deviceId, deviceKey}
func handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AdminToken string `json:"adminToken"`
		DeviceName string `json:"deviceName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "请求格式错误")
		return
	}
	if in.AdminToken != cfgAdminToken {
		fail(w, 401, "管理令牌错误")
		return
	}
	if in.DeviceName == "" {
		in.DeviceName = "未命名设备"
	}
	id, key, err := createDevice(in.DeviceName)
	if err != nil {
		fail(w, 500, "注册失败: "+err.Error())
		return
	}
	log.Printf("[register] 设备注册成功: %s (id=%d)", in.DeviceName, id)
	writeJSON(w, 200, map[string]interface{}{"deviceId": id, "deviceKey": key})
}

// ---------- 推送变更 ----------
// POST /api/push  {changes:[BlockChange...]}
// 冲突规则：客户端带 baseRev（它本地看到的该块版本），
// 若服务端该块的 rev > baseRev 且最后修改来自其他设备 => 冲突，保存两个版本等用户裁决。
func handlePush(dr *deviceReq) {
	var in struct {
		Changes []BlockChange `json:"changes"`
	}
	if err := json.NewDecoder(dr.Body).Decode(&in); err != nil {
		fail(dr.ResponseWriter, 400, "请求格式错误")
		return
	}
	type conflictOut struct {
		ConflictID int64       `json:"conflictId"`
		BlockID    string      `json:"blockId"`
		VersionA   *BlockChange `json:"versionA"`
		VersionB   *BlockChange `json:"versionB"`
	}
	var conflicts []conflictOut
	var applied int

	for i := range in.Changes {
		c := &in.Changes[i]
		rev, err := insertChange(c, dr.deviceID)
		if err != nil {
			log.Printf("[push] 写变更日志失败: %v", err)
			continue
		}
		// 查服务端该块的当前状态
		var curRev, curDev int64
		var has bool
		row := db.QueryRow("SELECT rev,device_id FROM blocks WHERE block_id=?", c.BlockID)
		if err := row.Scan(&curRev, &curDev); err == nil {
			has = true
		}
		if has && curRev > c.BaseRev && curDev != dr.deviceID {
			// 冲突：别的设备改过这块，而本地没见过那个版本
			verA, _ := getChangeByID(curRev)
			if cid, err := createConflict(c.BlockID, c.DocID, curRev, rev); err == nil {
				conflicts = append(conflicts, conflictOut{ConflictID: cid, BlockID: c.BlockID, VersionA: verA, VersionB: c})
				// 冲突期间块状态保持不变，等用户裁决后再写入
				log.Printf("[push] 检测到块冲突: %s (conflict=%d)", c.BlockID, cid)
				continue
			}
		}
		// 无冲突：更新最新块状态并广播给其他设备
		applyChange(c, dr.deviceID, rev)
		applied++
		hubRef.broadcastExcept(dr.deviceID, "remote_change", map[string]interface{}{
			"blockId": c.BlockID, "docId": c.DocID, "op": c.Op,
			"markdown": c.Markdown, "parentId": c.ParentID, "rev": rev,
		})
	}
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{
		"applied":   applied,
		"conflicts": conflicts,
	})
}

// ---------- 拉取变更（增量） ----------
// POST /api/pull {since: 上次收到的 rev} -> {changes:[...], latest: 最新rev}
func handlePull(dr *deviceReq) {
	var in struct {
		Since int64 `json:"since"`
	}
	json.NewDecoder(dr.Body).Decode(&in)
	rows, err := db.Query(`SELECT id,block_id,doc_id,op,markdown,parent_id FROM changes
		WHERE id>? AND device_id<>? ORDER BY id ASC LIMIT 5000`, in.Since, dr.deviceID)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []BlockChange
	var latest int64
	for rows.Next() {
		var c BlockChange
		var rev int64
		rows.Scan(&rev, &c.BlockID, &c.DocID, &c.Op, &c.Markdown, &c.ParentID)
		out = append(out, c)
		latest = rev
	}
	db.QueryRow("SELECT COALESCE(MAX(id),0) FROM changes").Scan(&latest)
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"changes": out, "latest": latest})
}

// ---------- 全量拉取 ----------
// POST /api/full {} -> 全部块最新状态（首次同步用）
func handleFull(dr *deviceReq) {
	blocks, err := getFullBlocks(0)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"blocks": blocks})
}

// ---------- WebSocket ----------
// GET /api/ws?device_key=xxx   长连接，接收 remote_change 推送
func handleWS(dr *deviceReq) {
	conn, err := upgrader.Upgrade(dr.ResponseWriter, dr.Request, nil)
	if err != nil {
		log.Printf("[ws] 升级失败: %v", err)
		return
	}
	hubRef.addConn(dr.deviceID, conn)
	// 读循环：客户端一般不发数据，只响应 ping/pong 保活
	go func() {
		defer func() {
			hubRef.removeConn(dr.deviceID, conn)
			conn.Close()
		}()
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(120 * time.Second))
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	// 写侧 ping 保活
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()
}

// ---------- 快照 ----------
// POST /api/snapshot/create {name}
func handleSnapshotCreate(dr *deviceReq) {
	var in struct {
		Name string `json:"name"`
	}
	json.NewDecoder(dr.Body).Decode(&in)
	id, err := createSnapshot(in.Name)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	log.Printf("[snapshot] 设备 %d 创建快照 #%d (%s)", dr.deviceID, id, in.Name)
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"id": id})
}

// GET /api/snapshot/list
func handleSnapshotList(dr *deviceReq) {
	list, err := listSnapshots()
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"snapshots": list})
}

// POST /api/snapshot/rollback {id} -> 返回快照时刻的全量块状态，插件负责写回本地
func handleSnapshotRollback(dr *deviceReq) {
	var in struct {
		ID int64 `json:"id"`
	}
	json.NewDecoder(dr.Body).Decode(&in)
	blocks, err := getFullBlocks(in.ID)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	log.Printf("[snapshot] 设备 %d 请求回滚到快照 #%d", dr.deviceID, in.ID)
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"blocks": blocks})
}

// ---------- 冲突 ----------
type ConflictView struct {
	ID       int64        `json:"id"`
	BlockID  string       `json:"blockId"`
	DocID    string       `json:"docId"`
	VersionA *BlockChange `json:"versionA"`
	VersionB *BlockChange `json:"versionB"`
	Created  int64        `json:"created"`
}

// GET /api/conflict/list -> 该设备需要处理的未解决冲突
func handleConflictList(dr *deviceReq) {
	rows, err := db.Query(`SELECT id,block_id,doc_id,version_a,version_b,created
		FROM conflicts WHERE status='open' ORDER BY id ASC`)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	var out []ConflictView
	for rows.Next() {
		var v ConflictView
		var verA, verB int64
		rows.Scan(&v.ID, &v.BlockID, &v.DocID, &verA, &verB, &v.Created)
		v.VersionA, _ = getChangeByID(verA)
		v.VersionB, _ = getChangeByID(verB)
		out = append(out, v)
	}
	rows.Close()
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"conflicts": out})
}

// POST /api/conflict/resolve {conflictId, chosenChangeId}
// 裁决后：把胜出版本写入 blocks 并广播给其他设备
func handleConflictResolve(dr *deviceReq) {
	var in struct {
		ConflictID int64 `json:"conflictId"`
		Chosen     int64 `json:"chosenChangeId"`
	}
	if err := json.NewDecoder(dr.Body).Decode(&in); err != nil {
		fail(dr.ResponseWriter, 400, "请求格式错误")
		return
	}
	var blockID, docID string
	var verA int64
	if err := db.QueryRow("SELECT block_id,doc_id,version_a FROM conflicts WHERE id=? AND status='open'", in.ConflictID).
		Scan(&blockID, &docID, &verA); err != nil {
		fail(dr.ResponseWriter, 404, "冲突不存在或已解决")
		return
	}
	chosen := in.Chosen
	if chosen == 0 {
		chosen = verA // 不选则默认保留服务端版本
	}
	c, err := getChangeByID(chosen)
	if err != nil {
		fail(dr.ResponseWriter, 404, "版本不存在")
		return
	}
	applyChange(c, dr.deviceID, chosen)
	db.Exec("UPDATE conflicts SET status='resolved', chosen=? WHERE id=?", chosen, in.ConflictID)
	// 把裁决结果广播出去（其他设备把该块更新为胜出版本）
	hubRef.broadcastExcept(dr.deviceID, "remote_change", map[string]interface{}{
		"blockId": c.BlockID, "docId": c.DocID, "op": c.Op,
		"markdown": c.Markdown, "parentId": c.ParentID, "rev": chosen,
	})
	log.Printf("[conflict] 冲突 #%d 已解决，块 %s 采用版本 %d", in.ConflictID, blockID, chosen)
	writeJSON(dr.ResponseWriter, 200, map[string]interface{}{"ok": true})
}

// ---------- Web 管理后台 ----------
// GET /admin 返回内嵌的单页管理界面（纯 HTML+JS，无外部依赖）
func handleAdminPage(w http.ResponseWriter, r *http.Request) {
	data, err := adminFS.ReadFile("web/admin.html")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// POST /api/admin/snapshot/create（后台用管理令牌创建快照）
func handleAdminSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	id, err := createSnapshot(in.Name)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("[snapshot] 管理后台创建快照 #%d (%s)", id, in.Name)
	writeJSON(w, 200, map[string]interface{}{"id": id})
}

// GET /api/admin/data?token=xxx -> 设备/最近变更/冲突/快照汇总
func handleAdminData(w http.ResponseWriter, r *http.Request) {
	type Device struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Created  int64  `json:"created"`
		LastSeen int64  `json:"lastSeen"`
		Online   bool   `json:"online"`
	}
	var devices []Device
	rows, _ := db.Query("SELECT id,name,created,last_seen FROM devices ORDER BY id")
	for rows.Next() {
		var d Device
		rows.Scan(&d.ID, &d.Name, &d.Created, &d.LastSeen)
		hubRef.mu.RLock()
		_, online := hubRef.conns[d.ID]
		hubRef.mu.RUnlock()
		d.Online = online
		devices = append(devices, d)
	}
	rows.Close()

	var changeCount, conflictCount int64
	db.QueryRow("SELECT COUNT(*) FROM changes").Scan(&changeCount)
	db.QueryRow("SELECT COUNT(*) FROM conflicts WHERE status='open'").Scan(&conflictCount)

	type LogRow struct {
		ID        int64  `json:"id"`
		BlockID   string `json:"blockId"`
		Op        string `json:"op"`
		DeviceID  int64  `json:"deviceId"`
		TS        int64  `json:"ts"`
		Markdown  string `json:"markdown"`
	}
	var logs []LogRow
	rows2, _ := db.Query(`SELECT id,block_id,op,device_id,ts,substr(markdown,1,200) FROM changes ORDER BY id DESC LIMIT 100`)
	for rows2.Next() {
		var l LogRow
		rows2.Scan(&l.ID, &l.BlockID, &l.Op, &l.DeviceID, &l.TS, &l.Markdown)
		logs = append(logs, l)
	}
	rows2.Close()

	snaps, _ := listSnapshots()
	var openConflicts []ConflictView
	rows3, _ := db.Query(`SELECT id,block_id,doc_id,version_a,version_b,created FROM conflicts WHERE status='open' ORDER BY id DESC LIMIT 50`)
	for rows3.Next() {
		var v ConflictView
		var verA, verB int64
		rows3.Scan(&v.ID, &v.BlockID, &v.DocID, &verA, &verB, &v.Created)
		v.VersionA, _ = getChangeByID(verA)
		v.VersionB, _ = getChangeByID(verB)
		openConflicts = append(openConflicts, v)
	}
	rows3.Close()

	writeJSON(w, 200, map[string]interface{}{
		"devices": devices, "changeCount": changeCount,
		"conflictCount": conflictCount, "logs": logs,
		"snapshots": snaps, "conflicts": openConflicts,
		"now": time.Now().Unix(),
	})
}

var _ = strconv.Itoa // 保留 import
