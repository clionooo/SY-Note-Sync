// SY Note Sync 服务端 - 附件（图片/音频/视频）同步
//
// 设计取舍：
//   · 思源的附件放在工作空间 data/assets/ 下，块里以 "assets/xxx.png" 形式引用。
//     所以附件用「路径」做键，不用块 ID。
//   · 客户端推块之前先调 /api/asset/check 问服务端缺哪些，只传缺的（省流量）。
//   · 文件本体存在 <数据目录>/files/<path>，数据库只存元信息（大小/哈希/类型）。
//   · 附件只增不删（块删了也留着），避免误删用户素材；要清理由后台手动删。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 单个附件上限（默认 200MB）。超过的直接拒收，避免把 NAS 塞爆。
const maxAssetBytes = 200 << 20

// assetDiskPath 把相对路径映射到磁盘路径，并挡住路径穿越
func assetDiskPath(rel string) (string, bool) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	rel = strings.TrimPrefix(rel, "/")
	rel = strings.TrimPrefix(rel, "data/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\x00") {
		return "", false
	}
	// 只允许 assets/ 开头（思源的附件目录），别的目录不碰
	if !strings.HasPrefix(rel, "assets/") {
		return "", false
	}
	full := filepath.Join(cfgDataDir, "files", filepath.FromSlash(rel))
	// 双保险：最终路径必须在 files 目录下
	base := filepath.Join(cfgDataDir, "files")
	if !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func assetMime(rel string) string {
	if m := mime.TypeByExtension(filepath.Ext(rel)); m != "" {
		return m
	}
	return "application/octet-stream"
}

// ---------------- 设备侧 ----------------

// POST /api/asset/check  {paths:[...]}  →  {missing:[...], total:n}
// 客户端在推块之前调一次，只传服务端没有的附件
func handleAssetCheck(dr *deviceReq) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(dr.Body).Decode(&in); err != nil {
		fail(dr.ResponseWriter, 400, "请求格式错误")
		return
	}
	clean := make([]string, 0, len(in.Paths))
	for _, p := range in.Paths {
		if _, ok := assetDiskPath(p); ok {
			clean = append(clean, strings.TrimPrefix(strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "/"), "data/"))
		}
	}
	missing := missingAssets(clean)
	ok(dr.ResponseWriter, map[string]interface{}{"missing": missing, "total": len(clean)})
}

// POST /api/asset/upload   multipart: path=assets/xxx.png & file=@xxx
func handleAssetUpload(dr *deviceReq) {
	// 限制请求体大小，防止有人拿它当网盘
	dr.Body = http.MaxBytesReader(dr.ResponseWriter, dr.Body, maxAssetBytes+1<<20)
	if err := dr.ParseMultipartForm(8 << 20); err != nil {
		fail(dr.ResponseWriter, 400, "解析上传内容失败（可能超过大小上限）: "+err.Error())
		return
	}
	rel := dr.FormValue("path")
	disk, valid := assetDiskPath(rel)
	if !valid {
		fail(dr.ResponseWriter, 400, "非法附件路径（只接受 assets/ 开头的相对路径）")
		return
	}
	file, header, err := dr.FormFile("file")
	if err != nil {
		fail(dr.ResponseWriter, 400, "缺少 file 字段")
		return
	}
	defer file.Close()
	if header.Size > maxAssetBytes {
		fail(dr.ResponseWriter, 413, "附件超过大小上限")
		return
	}
	if err := os.MkdirAll(filepath.Dir(disk), 0o755); err != nil {
		fail(dr.ResponseWriter, 500, "创建目录失败: "+err.Error())
		return
	}
	f, err := os.Create(disk)
	if err != nil {
		fail(dr.ResponseWriter, 500, "写入失败: "+err.Error())
		return
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), file)
	f.Close()
	if err != nil {
		fail(dr.ResponseWriter, 500, "写入失败: "+err.Error())
		return
	}
	rel = strings.TrimPrefix(strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/"), "data/")
	a := Asset{
		Path: rel, Size: n, Hash: hex.EncodeToString(h.Sum(nil)),
		Mime: assetMime(rel), DeviceID: dr.deviceID, Updated: now(),
	}
	if err := upsertAsset(a); err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	log.Printf("[asset] 设备 %d 上传 %s (%d 字节)", dr.deviceID, rel, n)
	// 通知其他在线设备：有新附件了，可以来拉
	hubRef.broadcastToDevices(dr.deviceID, map[string]interface{}{
		"type": "asset_added",
		"data": map[string]interface{}{"path": rel, "size": n},
	})
	ok(dr.ResponseWriter, map[string]interface{}{"ok": true, "path": rel, "size": n, "hash": a.Hash})
}

// GET|POST /api/asset/download?path=assets/xxx.png  → 文件字节流
func handleAssetDownload(dr *deviceReq) {
	rel := dr.URL.Query().Get("path")
	if rel == "" && dr.Method == http.MethodPost {
		var in struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(dr.Body).Decode(&in)
		rel = in.Path
	}
	disk, valid := assetDiskPath(rel)
	if !valid {
		fail(dr.ResponseWriter, 400, "非法附件路径")
		return
	}
	meta, exists := getAsset(strings.TrimPrefix(strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/"), "data/"))
	if !exists {
		fail(dr.ResponseWriter, 404, "服务端没有这个附件")
		return
	}
	f, err := os.Open(disk)
	if err != nil {
		fail(dr.ResponseWriter, 404, "附件文件缺失（可能被清理过）")
		return
	}
	defer f.Close()
	ct := meta.Mime
	if ct == "" {
		ct = assetMime(rel)
	}
	// 注意用 dr.ResponseWriter：deviceReq 同时嵌了 ResponseWriter 和 *Request，
	// 直接写 dr.Header / dr.Write 会报 ambiguous selector
	w := dr.ResponseWriter
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", itoa(meta.Size))
	w.Header().Set("X-Asset-Hash", meta.Hash)
	_, _ = io.Copy(w, f)
}

// POST /api/asset/list  →  {assets:[{path,size,hash,updated}]}
// 新设备首次同步时用它一次性列出服务端全部附件
func handleAssetList(dr *deviceReq) {
	list, err := listAssets(5000)
	if err != nil {
		fail(dr.ResponseWriter, 500, err.Error())
		return
	}
	ok(dr.ResponseWriter, map[string]interface{}{"assets": list})
}

// POST /api/asset/delete  {paths:[...]}
func handleAssetDelete(dr *deviceReq) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(dr.Body).Decode(&in); err != nil {
		fail(dr.ResponseWriter, 400, "请求格式错误")
		return
	}
	n := 0
	for _, p := range in.Paths {
		disk, valid := assetDiskPath(p)
		if !valid {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "/"), "data/")
		_ = deleteAsset(rel)
		_ = os.Remove(disk)
		n++
	}
	log.Printf("[asset] 设备 %d 删除 %d 个附件", dr.deviceID, n)
	hubRef.broadcastToDevices(dr.deviceID, map[string]interface{}{"type": "asset_removed", "paths": in.Paths})
	ok(dr.ResponseWriter, map[string]interface{}{"ok": true, "deleted": n})
}

// ---------------- 管理后台 ----------------

// GET /api/admin/assets?limit=500
func handleAdminAssets(w http.ResponseWriter, r *http.Request) {
	list, err := listAssets(int(atoiDefault(r.URL.Query().Get("limit"), 500)))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	count, bytes := assetStats()
	ok(w, map[string]interface{}{"assets": list, "count": count, "bytes": bytes})
}

// POST /api/admin/asset/delete {paths:[...]}
func handleAdminAssetDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := decode(r, &in); err != nil || len(in.Paths) == 0 {
		fail(w, 400, "参数错误")
		return
	}
	n := 0
	for _, p := range in.Paths {
		disk, valid := assetDiskPath(p)
		if !valid {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "/"), "data/")
		_ = deleteAsset(rel)
		_ = os.Remove(disk)
		n++
	}
	log.Printf("[asset] 管理后台删除 %d 个附件", n)
	hubRef.broadcastToDevices(0, map[string]interface{}{"type": "asset_removed", "paths": in.Paths})
	ok(w, map[string]interface{}{"ok": true, "deleted": n})
}

// GET /api/admin/asset/download?path=...  后台直接下载看
func handleAdminAssetDownload(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	disk, valid := assetDiskPath(rel)
	if !valid {
		fail(w, 400, "非法附件路径")
		return
	}
	f, err := os.Open(disk)
	if err != nil {
		fail(w, 404, "文件不存在")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", assetMime(rel))
	_, _ = io.Copy(w, f)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
