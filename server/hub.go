// SY Note Sync 服务端 - WebSocket 枢纽
//
// 两条独立的连接池：
//   devices —— 各端思源插件，用来把变更实时推给其他设备
//   admins  —— 管理后台页面，用来实时刷新看板/日志
package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn 对底层连接的薄封装：带写锁，避免并发写同一个 conn 导致 panic
type wsConn struct {
	c    *websocket.Conn
	mu   sync.Mutex
	kind string // "device" | "admin"
	id   int64  // device id；admin 为 0
}

func (w *wsConn) writeJSON(v interface{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.c.WriteJSON(v)
}

func (w *wsConn) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.c.WriteMessage(websocket.PingMessage, nil)
}

type hub struct {
	mu      sync.RWMutex
	devices map[int64]*wsConn // 一个设备同时只保留一条连接（新连接顶掉旧的）
	admins  map[*wsConn]bool
}

var hubRef *hub

func newHub() *hub {
	return &hub{devices: map[int64]*wsConn{}, admins: map[*wsConn]bool{}}
}

// ---- 设备连接 ----

func (h *hub) addDevice(id int64, c *wsConn) {
	h.mu.Lock()
	if old, ok := h.devices[id]; ok && old != c {
		go old.c.Close() // 顶掉旧连接，避免同一设备两条长连接重复收推送
	}
	h.devices[id] = c
	n := len(h.devices)
	h.mu.Unlock()
	touchDevice(id)
	log.Printf("[hub] 设备 %d 已连接 (在线设备数: %d)", id, n)
	h.notifyAdmins(map[string]interface{}{"type": "device_online", "deviceId": id, "online": true})
}

func (h *hub) removeDevice(id int64, c *wsConn) {
	h.mu.Lock()
	if cur, ok := h.devices[id]; ok && cur == c {
		delete(h.devices, id)
	}
	n := len(h.devices)
	h.mu.Unlock()
	log.Printf("[hub] 设备 %d 已断开 (在线设备数: %d)", id, n)
	h.notifyAdmins(map[string]interface{}{"type": "device_online", "deviceId": id, "online": false})
}

func (h *hub) isOnline(id int64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.devices[id]
	return ok
}

func (h *hub) onlineIDs() map[int64]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[int64]bool, len(h.devices))
	for id := range h.devices {
		out[id] = true
	}
	return out
}

// broadcastToDevices 把一条远端变更推给除 origin 之外的所有在线设备
func (h *hub) broadcastToDevices(origin int64, payload interface{}) {
	msg, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*wsConn, 0, len(h.devices))
	for id, c := range h.devices {
		if id == origin {
			continue
		}
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.mu.Lock()
		_ = c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := c.c.WriteMessage(websocket.TextMessage, msg)
		c.mu.Unlock()
		if err != nil {
			log.Printf("[hub] 推送设备 %d 失败: %v", c.id, err)
		}
	}
}

// ---- 管理后台连接 ----

func (h *hub) addAdmin(c *wsConn) {
	h.mu.Lock()
	h.admins[c] = true
	h.mu.Unlock()
}

func (h *hub) removeAdmin(c *wsConn) {
	h.mu.Lock()
	delete(h.admins, c)
	h.mu.Unlock()
}

func (h *hub) notifyAdmins(v interface{}) {
	h.mu.RLock()
	targets := make([]*wsConn, 0, len(h.admins))
	for c := range h.admins {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		if err := c.writeJSON(v); err != nil {
			h.removeAdmin(c)
			c.c.Close()
		}
	}
}

// keepAlive 定期给所有连接发 ping，顺便踢掉死连接
func (h *hub) keepAlive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.RLock()
		conns := make([]*wsConn, 0, len(h.devices)+len(h.admins))
		for _, c := range h.devices {
			conns = append(conns, c)
		}
		for c := range h.admins {
			conns = append(conns, c)
		}
		h.mu.RUnlock()
		for _, c := range conns {
			if err := c.ping(); err != nil {
				c.c.Close()
			}
		}
	}
}
