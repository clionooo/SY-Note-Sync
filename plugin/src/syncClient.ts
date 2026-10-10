/**
 * SY Note Sync - 同步客户端模块
 * 职责：与中心化同步服务通信（HTTP 推拉 + WebSocket 接收）
 * 注意：本模块不直接访问思源内核，思源 API 的调用统一在 index.ts 完成
 */

/** 服务端块变更结构（与后端 BlockChange 对应） */
export interface BlockChange {
    blockId: string;   // 思源块 ID（同步最小单元）
    docId: string;     // 所属文档 ID
    op: "insert" | "update" | "delete"; // 操作类型
    markdown: string;  // 块的 markdown 内容（删除时为空）
    parentId?: string; // 父块 ID（插入时定位用）
    baseRev?: number;  // 本地已知的该块版本号（冲突检测依据）
    rev?: number;      // 服务端分配的版本号
}

/** 推送响应中的冲突描述 */
export interface ConflictInfo {
    conflictId: number;
    blockId: string;
    docId: string;
    versionA: BlockChange; // 服务端当前版本
    versionB: BlockChange; // 本地挑战版本
}

/** 设备注册响应 */
export interface RegisterResult {
    deviceId: number;
    deviceKey: string;
}

export class SyncClient {
    private ws: WebSocket | null = null;
    private lastRev = 0;          // 最近收到的远端版本号（增量拉取游标）
    private reconnectTimer: any = null;
    private alive = false;        // WebSocket 连接状态

    constructor(
        private serverUrl: string, // 同步服务地址，如 http://192.168.1.10:8282 或 https://sync.example.com
        private deviceKey: string, // 设备密钥
        private onRemoteChange: (c: BlockChange) => void, // 收到远端变更的回调
        private onStatusChange: (online: boolean) => void, // 连接状态变化回调
    ) {
        // 规范化地址：去掉末尾斜杠
        this.serverUrl = serverUrl.replace(/\/+$/, "");
    }

    /** 通用 POST 请求：自动附带设备密钥 */
    private async post<T = any>(path: string, body?: any): Promise<T> {
        const res = await fetch(this.serverUrl + path, {
            method: "POST",
            headers: {
                "Content-Type": "application/json",
                "X-Device-Key": this.deviceKey,
            },
            body: JSON.stringify(body || {}),
        });
        const data = await res.json();
        if (!res.ok) {
            throw new Error((data as any).error || `HTTP ${res.status}`);
        }
        return data as T;
    }

    // ---------------- 设备注册 ----------------
    /** 用管理令牌注册新设备，返回分配的设备密钥（一次性操作，密钥要保存） */
    static async register(serverUrl: string, adminToken: string, deviceName: string): Promise<RegisterResult> {
        const res = await fetch(serverUrl.replace(/\/+$/, "") + "/api/register", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ adminToken, deviceName }),
        });
        const data = await res.json();
        if (!res.ok) throw new Error((data as any).error || `HTTP ${res.status}`);
        return data as RegisterResult;
    }

    // ---------------- 推送 ----------------
    /** 批量推送本地块变更，返回服务端检测出的冲突 */
    async push(changes: BlockChange[]): Promise<{ applied: number; conflicts: ConflictInfo[] }> {
        return this.post("/api/push", { changes });
    }

    // ---------------- 拉取 ----------------
    /** 增量拉取 since 之后的远端变更 */
    async pull(since: number): Promise<{ changes: BlockChange[]; latest: number }> {
        return this.post("/api/pull", { since });
    }

    /** 全量拉取（首次同步）：返回所有块的最新状态 */
    async full(): Promise<{ blocks: BlockChange[] }> {
        return this.post("/api/full", {});
    }

    // ---------------- 快照 ----------------
    async createSnapshot(name: string) {
        return this.post("/api/snapshot/create", { name });
    }

    async listSnapshots() {
        return this.post("/api/snapshot/list", {});
    }

    /** 请求回滚：服务端返回快照时的全量块状态，由插件写回本地 */
    async rollback(id: number): Promise<{ blocks: BlockChange[] }> {
        return this.post("/api/snapshot/rollback", { id });
    }

    // ---------------- 冲突 ----------------
    async listConflicts(): Promise<{ conflicts: ConflictInfo[] }> {
        return this.post("/api/conflict/list", {});
    }

    /** 解决冲突：chosenChangeId 传 0 表示保留服务端版本 */
    async resolveConflict(conflictId: number, chosenChangeId: number) {
        return this.post("/api/conflict/resolve", { conflictId, chosenChangeId });
    }

    // ---------------- WebSocket 长连接 ----------------
    /**
     * 建立 WS 连接接收远端变更。
     * WS 地址转换：http->ws / https->wss，附带 device_key 参数鉴权
     */
    connect() {
        if (!this.serverUrl || !this.deviceKey) return;
        const wsUrl = this.serverUrl
            .replace(/^http/, "ws")
            + "/api/ws?device_key=" + encodeURIComponent(this.deviceKey);
        try {
            this.ws = new WebSocket(wsUrl);
        } catch (e) {
            console.error("SY Note Sync: WS 连接失败", e);
            this.scheduleReconnect();
            return;
        }
        this.ws.onopen = () => {
            this.alive = true;
            this.onStatusChange(true);
            console.log("SY Note Sync: WebSocket 已连接");
            // 连上后做一次增量拉取，补齐断线期间错过的变更
            this.pull(this.lastRev).then(r => {
                this.lastRev = r.latest || this.lastRev;
                (r.changes || []).forEach(c => this.onRemoteChange(c));
            }).catch(console.error);
        };
        this.ws.onmessage = (ev) => {
            try {
                const msg = JSON.parse(ev.data);
                if (msg.type === "remote_change") {
                    const c = msg.data as BlockChange;
                    if (c.rev && c.rev > this.lastRev) this.lastRev = c.rev;
                    this.onRemoteChange(c);
                }
            } catch (e) {
                console.error("SY Note Sync: WS 消息解析失败", e);
            }
        };
        this.ws.onclose = () => {
            this.alive = false;
            this.onStatusChange(false);
            this.scheduleReconnect();
        };
        this.ws.onerror = () => {
            this.alive = false;
        };
    }

    /** 断线自动重连（5 秒退避） */
    private scheduleReconnect() {
        if (this.reconnectTimer) return;
        this.reconnectTimer = setTimeout(() => {
            this.reconnectTimer = null;
            this.connect();
        }, 5000);
    }

    /** 是否在线 */
    get online() { return this.alive; }

    /** 主动断开（暂停同步时调用） */
    disconnect() {
        if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null; }
        if (this.ws) { this.ws.onclose = null; this.ws.close(); this.ws = null; }
        this.alive = false;
        this.onStatusChange(false);
    }
}
