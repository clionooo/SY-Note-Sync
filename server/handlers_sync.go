// SY Note Sync 服务端 - 设备侧接口（插件调用）
//
// 鉴权：请求头 X-Device-Key（WebSocket 也允许 ?device_key= 便于移动端）
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// ---------------- 通用响应工具 ----------------

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func ok(w http.ResponseWriter, v interface{}) { writeJSON(w, 200, v) }

// deviceReq 把「已鉴权的设备」和原始请求绑在一起往下传
type deviceReq struct {
	http.ResponseWriter
	*http.Request
	deviceID int64
}

// authDevice 中间件：校验设备密钥
func authDevice(next func(*deviceReq)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Device-Key")
		if key == "" {
			// 移动端 WebSocket 不方便自定义头，允许 URL 参数
			key = r.URL.Query().Get("device_key")
		}
		if key == "" {
			fail(w, 401, "缺少设备密钥")
			return
		}
		d, err := deviceByKey(key)
		if err != nil {
			fail(w, 401, "设备密钥无效")
			return
		}
		touchDevice(d.ID)
		next(&deviceReq{ResponseWriter: w, Request: r, deviceID: d.ID})
	}
}

// ---------------- 设备注册 ----------------

// POST /api/register {adminToken, deviceName, platform, version}
// 返回 {deviceId, deviceKey, serverVersion, protocol}
func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只支持 POST")
		return
	}
	// 服务端设置里可以关掉注册（部署给别人用时防止乱注册）
	if getSetting("allow_register", "1") != "1" {
		fail(w, 403, "服务端已关闭新设备注册")
		return
	}
	var in struct {
		AdminToken string `json:"adminToken"`
		DeviceName string `json:"deviceName"`
		Platform   string `json:"platform"`
		Version    string `json:"version"`
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
	setDeviceInfo(id, in.Platform, in.Version)
	log.Printf("[register] 设备注册成功: %s (id=%d, platform=%s)", in.DeviceName, id, in.Platform)
	hubRef.notifyAdmins(map[string]interface{}{"type": "device_added", "deviceId": id, "name": in.DeviceName})
	ok(w, map[string]interface{}{
		"deviceId":      id,
		"deviceKey":     key,
		"serverVersion": ServerVersion,
		"protocol":      ProtocolVersion,
	})
}

// POST /api/hello  {platform, version}
// 插件每次连接时上报自己的平台/版本，顺便拿到服务端版本做版本提示
func handleHello(dr *deviceReq) {
	var in struct {
		Platform string `json:"platform"`
		Version  string `json:"version"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	setDeviceInfo(dr.deviceID, in.Platform, in.Version)
	d, _ := getDevice(dr.deviceID)
	ok(dr.ResponseWriter, map[string]interface{}{
		"serverVersion":   ServerVersion,
		"protocol":        ProtocolVersion,
		"minPlugin":       MinPluginVersion,
		"device":          d,
		"upgradeRequired": in.Version != "" && in.Version < MinPluginVersion,
	})
}

// ---------------- 推送 ----------------

// POST /api/push {changes:[...]}
// 冲突规则：客户端带上它本地看到的该块版本 baseRev，
// 如果服务端该块的 rev 比 baseRev 大、且最后写入者不是本设备 => 判定冲突，
// 两个版本都留着，等用户在插件里选一个。
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
		DocID      string      `json:"docId"`
		VersionA   BlockChange `json:"versionA"`
		VersionB   BlockChange `json:"versionB"`
	}
	applied := 0
	var conflicts []conflictOut
	ts := now()

	for _, c := range in.Changes {
		if c.BlockID == "" || c.Op == "" {
			continue
		}
		// 1) 先落流水（历史永远留痕，即使后面判定冲突）
		rev, err := appendChange(dr.deviceID, c, ts)
		if err != nil {
			log.Printf("[push] 写变更日志失败: %v", err)
			continue
		}
		c.Rev = rev

		// 2) 冲突判定（force 的变更跳过：那是用户主动发起的整体覆盖）
		cur, exists, _ := getBlock(c.BlockID)
		if !c.Force && exists && c.BaseRev > 0 && cur.Rev > c.BaseRev && cur.DeviceID != dr.deviceID {
			// 服务端版本更新、而且不是我改的 => 冲突，两个版本都留
			cid, err := createConflict(c.BlockID, c.DocID, BlockChange{
				BlockID: cur.BlockID, DocID: cur.DocID, Op: cur.Op,
				Content: cur.Content, ParentID: cur.ParentID, Rev: cur.Rev,
			}, c)
			if err == nil {
				log.Printf("[push] 检测到块冲突: %s (conflict=%d)", c.BlockID, cid)
				conflicts = append(conflicts, conflictOut{
					ConflictID: cid, BlockID: c.BlockID, DocID: c.DocID,
					VersionA: BlockChange{BlockID: cur.BlockID, DocID: cur.DocID, Op: cur.Op,
						Content: cur.Content, ParentID: cur.ParentID, Rev: cur.Rev},
					VersionB: c,
				})
				hubRef.notifyAdmins(map[string]interface{}{"type": "conflict", "blockId": c.BlockID})
				continue // 冲突的变更不落地，等用户裁决
			}
		}

		// 3) 更新块最新状态
		if err := upsertBlock(dr.deviceID, c, rev, ts); err != nil {
			log.Printf("[push] 更新块状态失败: %v", err)
			continue
		}
		// 4) 顺手维护文档/笔记本索引
		maintainMeta(c)
		applied++

		// 5) 实时广播给其他设备
		hubRef.broadcastToDevices(dr.deviceID, map[string]interface{}{
			"type": "remote_change",
			"data": c,
		})
	}

	hubRef.notifyAdmins(map[string]interface{}{"type": "changes", "applied": applied})
	ok(dr.ResponseWriter, map[string]interface{}{
		"applied":   applied,
		"conflicts": conflicts,
		"latest":    latestRev(),
	})
}

// maintainMeta 根据变更内容维护 docs / notebooks 索引
func maintainMeta(c BlockChange) {
	if c.NotebookID != "" {
		_ = upsertNotebook(c.NotebookID, "", "")
	}
	if c.DocID != "" {
		deleted := c.Op == "delete" && c.BlockID == c.DocID
		_ = upsertDoc(c.DocID, c.NotebookID, "", deleted)
	}
}

// ---------------- 拉取 ----------------

// POST /api/pull {since}  增量拉取
func handlePull(dr *deviceReq) {
	var in struct {
		Since int64 `json:"since"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	changes, latest, err := changesSince(in.Since, dr.deviceID, MaxPullLimit)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"changes": changes, "latest": latest})
}

// POST /api/full {}  全量拉取（新设备首次同步 / 手动修复）
func handleFull(dr *deviceReq) {
	blocks, err := allBlocks(false)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"blocks": blocks, "latest": latestRev()})
}

// ---------------- WebSocket ----------------

func handleWS(dr *deviceReq) {
	conn, err := upgrader.Upgrade(dr.ResponseWriter, dr.Request, nil)
	if err != nil {
		log.Printf("[ws] 升级失败: %v", err)
		return
	}
	wc := &wsConn{c: conn, kind: "device", id: dr.deviceID}
	hubRef.addDevice(dr.deviceID, wc)
	defer func() {
		hubRef.removeDevice(dr.deviceID, wc)
		conn.Close()
	}()
	// 连上先告诉插件「当前最新版本号」，插件可据此判断要不要补拉
	_ = wc.writeJSON(map[string]interface{}{"type": "hello", "latest": latestRev(), "server": ServerVersion})

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// 目前不需要处理上行消息，读循环只为探活与触发断开
	}
}

// ---------------- 快照 ----------------

func handleSnapshotCreate(dr *deviceReq) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	if in.Name == "" {
		in.Name = "手动快照"
	}
	id, err := createSnapshot(in.Name, false, "")
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	log.Printf("[snapshot] 设备 %d 创建快照 #%d (%s)", dr.deviceID, id, in.Name)
	ok(dr.ResponseWriter, map[string]interface{}{"id": id})
}

func handleSnapshotList(dr *deviceReq) {
	list, err := listSnapshots()
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"snapshots": list})
}

// handleSnapshotRollback 返回快照时刻的全部块，由插件写回本地
func handleSnapshotRollback(dr *deviceReq) {
	var in struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	blocks, err := snapshotBlocks(in.ID)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	log.Printf("[snapshot] 设备 %d 请求回滚到快照 #%d", dr.deviceID, in.ID)
	ok(dr.ResponseWriter, map[string]interface{}{"blocks": blocks})
}

// ---------------- 冲突 ----------------

func handleConflictList(dr *deviceReq) {
	list, err := listConflicts(true)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"conflicts": list})
}

// handleConflictResolve chosenRev=0 表示保留服务端版本(A)，否则保留挑战版本(B)
func handleConflictResolve(dr *deviceReq) {
	var in struct {
		ConflictID   int64 `json:"conflictId"`
		ChosenChange int64 `json:"chosenChangeId"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	c, err := getConflict(in.ConflictID)
	if err != nil {
		fail(dr.ResponseWriter, 404, "冲突不存在")
		return
	}
	chosen := c.VersionA
	if in.ChosenChange != 0 {
		chosen = c.VersionB
	}
	rev := latestRev() + 1
	if err := upsertBlock(dr.deviceID, chosen, rev, now()); err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	_ = markConflictResolved(in.ConflictID, chosen.Rev)
	log.Printf("[conflict] 冲突 #%d 已解决，块 %s 采用版本 %d", in.ConflictID, c.BlockID, chosen.Rev)
	hubRef.broadcastToDevices(dr.deviceID, map[string]interface{}{"type": "remote_change", "data": chosen})
	ok(dr.ResponseWriter, map[string]interface{}{"ok": true, "block": chosen})
}

// ---------------- 回收站 / 块历史 ----------------

func handleTrash(dr *deviceReq) {
	list, err := trash()
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"trash": list})
}

func handleBlockHistory(dr *deviceReq) {
	var in struct {
		BlockID string `json:"blockId"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	if in.BlockID == "" {
		fail(dr.ResponseWriter, 400, "缺少 blockId")
		return
	}
	list, err := blockHistory(in.BlockID, 100)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"history": list})
}

// handleBlockRestore 把某个块恢复到指定历史版本，并广播给其他设备
func handleBlockRestore(dr *deviceReq) {
	var in struct {
		ChangeID int64 `json:"changeId"`
	}
	_ = json.NewDecoder(dr.Body).Decode(&in)
	rc, err := getChange(in.ChangeID)
	if err != nil {
		fail(dr.ResponseWriter, 404, "变更记录不存在")
		return
	}
	op := rc.Op
	if op == "delete" {
		op = "insert" // 恢复被删的块 => 对本地来说是重新插入
	}
	block := BlockChange{
		BlockID: rc.BlockID, DocID: rc.DocID, NotebookID: rc.NotebookID,
		Op: op, Content: rc.Content,
	}
	rev, err := appendChange(dr.deviceID, block, now())
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	block.Rev = rev
	cur, _, _ := getBlock(rc.BlockID)
	block.ParentID = cur.ParentID
	if err := upsertBlock(dr.deviceID, block, rev, now()); err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	log.Printf("[restore] 设备 %d 恢复块 %s 到版本 %d (op=%s)", dr.deviceID, rc.BlockID, in.ChangeID, op)
	hubRef.broadcastToDevices(dr.deviceID, map[string]interface{}{"type": "remote_change", "data": block})
	ok(dr.ResponseWriter, map[string]interface{}{"ok": true, "block": block})
}

// ---------------- 元数据上报 ----------------

// POST /api/meta {notebooks:[{id,name,icon}], docs:[{id,notebookId,title,deleted}]}
// 插件定期上报笔记本/文档结构，管理后台就能显示目录树
func handleMeta(dr *deviceReq) {
	var in struct {
		Notebooks []Notebook `json:"notebooks"`
		Docs      []Doc      `json:"docs"`
	}
	if err := json.NewDecoder(dr.Body).Decode(&in); err != nil {
		fail(dr.ResponseWriter, 400, "请求格式错误")
		return
	}
	for _, n := range in.Notebooks {
		_ = upsertNotebook(n.ID, n.Name, n.Icon)
	}
	for _, d := range in.Docs {
		_ = upsertDoc(d.ID, d.NotebookID, d.Title, d.Deleted)
	}
	ok(dr.ResponseWriter, map[string]interface{}{
		"ok":        true,
		"notebooks": len(in.Notebooks),
		"docs":      len(in.Docs),
	})
}

// ---------------- 管理后台实时通道 ----------------

func handleAdminWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != cfgAdminToken {
		fail(w, 401, "管理令牌错误")
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	wc := &wsConn{c: conn, kind: "admin"}
	hubRef.addAdmin(wc)
	defer func() {
		hubRef.removeAdmin(wc)
		conn.Close()
	}()
	_ = wc.writeJSON(map[string]interface{}{"type": "hello", "server": ServerVersion, "stats": stats(cfgDBPath)})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// atoiDefault 解析 query 里的整数
func atoiDefault(s string, def int64) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
