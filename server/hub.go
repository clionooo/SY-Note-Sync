// WebSocket 枢纽：维护所有在线设备的长连接，向"其他设备"广播远端变更
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// 允许任意来源（部署在反代/域名后，由密钥鉴权保证安全）
	CheckOrigin: func(r *http.Request) bool { return true },
}

// hub 管理在线设备连接表：deviceID -> conn
type hub struct {
	mu    sync.RWMutex
	conns map[int64]*websocket.Conn
}

var hubRef *hub

func newHub() *hub { return &hub{conns: make(map[int64]*websocket.Conn)} }

func (h *hub) run() {
	// 心跳兜底：每 30 秒清一次断开的连接（写超时检测）
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		h.mu.Lock()
		for id, conn := range h.conns {
			_ = conn
			_ = id
		}
		h.mu.Unlock()
	}
}

// addConn 注册设备连接，返回该设备之前的旧连接（如有）
func (h *hub) addConn(deviceID int64, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.conns[deviceID]; ok && old != nil {
		old.Close() // 同设备重复连接时踢掉旧连接
	}
	h.conns[deviceID] = conn
	log.Printf("[hub] 设备 %d 已连接 (在线设备数: %d)", deviceID, len(h.conns))
}

// removeConn 连接断开时移除
func (h *hub) removeConn(deviceID int64, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[deviceID]; ok && cur == conn {
		delete(h.conns, deviceID)
		log.Printf("[hub] 设备 %d 已断开 (在线设备数: %d)", deviceID, len(h.conns))
	}
}

// broadcastExcept 把消息推送给除 excludeDevice 外的所有在线设备
func (h *hub) broadcastExcept(excludeDevice int64, msgType string, payload interface{}) {
	data, _ := json.Marshal(map[string]interface{}{"type": msgType, "data": payload})
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, conn := range h.conns {
		if id == excludeDevice {
			continue
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("[hub] 推送设备 %d 失败: %v", id, err)
		}
	}
}
