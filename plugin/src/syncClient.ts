/**
 * SY Note Sync · 同步客户端
 *
 * 只负责「和同步服务说话」：HTTP 推拉 + WebSocket 收推送。
 * 不碰思源内核 API（那是 index.ts 的事），这样两端都能单独测。
 */

/** 一个块的变更。content 是思源的块 DOM（带 data-node-id），不是 markdown。 */
export interface BlockChange {
    blockId: string;
    docId?: string;
    notebookId?: string;
    op: "insert" | "update" | "delete";
    content: string;
    parentId?: string;
    baseRev?: number;
    rev?: number;
    /** 以本机为准，服务端跳过冲突判定（「全量上传」用） */
    force?: boolean;
}

export interface ConflictInfo {
    id: number;
    blockId: string;
    docId: string;
    created: number;
    versionA: BlockChange;
    versionB: BlockChange;
}

export interface TrashItem {
    id: number;
    blockId: string;
    docId: string;
    deviceId: number;
    deviceName?: string;
    ts: number;
    content: string;
}

export interface HistoryItem {
    id: number;
    blockId: string;
    docId: string;
    op: string;
    deviceId: number;
    deviceName?: string;
    ts: number;
    content: string;
}

export interface SnapshotInfo {
    id: number;
    name: string;
    blocks: number;
    created: number;
    note?: string;
    auto?: boolean;
}

export interface RegisterResult {
    deviceId: number;
    deviceKey: string;
    serverVersion: string;
    protocol: number;
}

export interface HelloResult {
    serverVersion: string;
    protocol: number;
    minPlugin: string;
    upgradeRequired: boolean;
}

export interface ClientHooks {
    onRemoteChange?: (c: BlockChange) => void;
    onStatus?: (online: boolean) => void;
    onRev?: (rev: number) => void;
    /** 服务端要求重新全量同步（例如后台回滚了快照） */
    onReload?: (reason: string) => void;
    /** 附件事件：kind = asset_added / asset_removed */
    onAssetEvent?: (kind: string, data: any) => void;
    onLog?: (level: "info" | "warn" | "error", msg: string) => void;
}

const DEFAULT_TIMEOUT = 15000;

/** 带超时的 fetch —— 移动端网络不稳，没超时会出现「点了没反应」的假死 */
async function fetchTimeout(url: string, init: RequestInit = {}, timeout = DEFAULT_TIMEOUT): Promise<Response> {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeout);
    try {
        return await fetch(url, { ...init, signal: ctrl.signal });
    } finally {
        clearTimeout(timer);
    }
}

/** 把底层异常翻译成人能看懂的话（iOS 上 Failed to fetch 太含糊了） */
function humanError(e: any, url: string): string {
    const msg = (e && e.message) || String(e);
    if (e && e.name === "AbortError") return `请求超时（${url}），检查服务地址和网络`;
    if (/Failed to fetch|NetworkError|Load failed/i.test(msg)) {
        return `连不上服务端（${url}）。常见原因：地址/端口写错、手机不在同一网络、服务未启动、被防火墙拦`;
    }
    return msg;
}

export class SyncClient {
    private ws: WebSocket | null = null;
    private lastRev = 0;
    private reconnectTimer: any = null;
    private alive = false;
    private closed = false;
    private hooks: ClientHooks;
    private pingTimer: any = null;

    constructor(
        private serverUrl: string,
        private deviceKey: string,
        hooks: ClientHooks = {},
        initialRev = 0,
    ) {
        this.serverUrl = (serverUrl || "").trim().replace(/\/+$/, "");
        this.hooks = hooks;
        this.lastRev = initialRev || 0;
    }

    get online() { return this.alive; }
    get rev() { return this.lastRev; }

    private log(level: "info" | "warn" | "error", msg: string) {
        if (this.hooks.onLog) this.hooks.onLog(level, msg);
    }

    private setRev(rev: number) {
        if (!rev || rev <= this.lastRev) return;
        this.lastRev = rev;
        if (this.hooks.onRev) this.hooks.onRev(rev);
    }

    /** 通用 POST：自动带设备密钥 + 统一错误翻译 */
    private async post<T = any>(path: string, body?: any, timeout = DEFAULT_TIMEOUT): Promise<T> {
        const url = this.serverUrl + path;
        let res: Response;
        try {
            res = await fetchTimeout(url, {
                method: "POST",
                headers: {
                    "Content-Type": "application/json",
                    "X-Device-Key": this.deviceKey,
                },
                body: JSON.stringify(body || {}),
            }, timeout);
        } catch (e: any) {
            throw new Error(humanError(e, url));
        }
        let data: any = null;
        try { data = await res.json(); } catch { /* 可能空响应 */ }
        if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
        return data as T;
    }

    // ---------------- 注册 ----------------
    /** 用管理令牌注册设备，拿回设备密钥（一台设备只做一次） */
    static async register(serverUrl: string, adminToken: string, deviceName: string,
                          platform = "", version = ""): Promise<RegisterResult> {
        const base = (serverUrl || "").trim().replace(/\/+$/, "");
        const url = base + "/api/register";
        let res: Response;
        try {
            res = await fetchTimeout(url, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ adminToken, deviceName, platform, version }),
            });
        } catch (e: any) {
            throw new Error(humanError(e, url));
        }
        let data: any = null;
        try { data = await res.json(); } catch { /* ignore */ }
        if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
        return data as RegisterResult;
    }

    /** 连接性自检：不需要设备密钥，用来判断「地址通不通」 */
    static async ping(serverUrl: string): Promise<{ serverVersion: string; serverName: string; protocol: number }> {
        const base = (serverUrl || "").trim().replace(/\/+$/, "");
        const url = base + "/api/version";
        let res: Response;
        try {
            res = await fetchTimeout(url, { method: "GET" }, 8000);
        } catch (e: any) {
            throw new Error(humanError(e, url));
        }
        if (!res.ok) throw new Error(`服务端返回 HTTP ${res.status}`);
        return await res.json();
    }

    // ---------------- 同步 ----------------
    hello(platform: string, version: string): Promise<HelloResult> {
        return this.post<HelloResult>("/api/hello", { platform, version }, 8000);
    }
    push(changes: BlockChange[]): Promise<{ applied: number; conflicts: ConflictInfo[]; latest: number }> {
        return this.post("/api/push", { changes });
    }
    pull(since: number): Promise<{ changes: BlockChange[]; latest: number }> {
        return this.post("/api/pull", { since });
    }
    full(): Promise<{ blocks: BlockChange[]; latest: number }> {
        return this.post("/api/full", {});
    }
    reportMeta(payload: { notebooks: any[]; docs: any[] }) {
        return this.post("/api/meta", payload);
    }

    // ---------------- 快照 ----------------
    createSnapshot(name: string) { return this.post<{ id: number }>("/api/snapshot/create", { name }); }
    listSnapshots() { return this.post<{ snapshots: SnapshotInfo[] }>("/api/snapshot/list", {}); }
    rollbackSnapshot(id: number) { return this.post<{ blocks: BlockChange[] }>("/api/snapshot/rollback", { id }); }

    // ---------------- 冲突 ----------------
    listConflicts() { return this.post<{ conflicts: ConflictInfo[] }>("/api/conflict/list", {}); }
    resolveConflict(conflictId: number, chosenChangeId: number) {
        return this.post("/api/conflict/resolve", { conflictId, chosenChangeId });
    }

    // ---------------- 回收站 / 历史 ----------------
    trash() { return this.post<{ trash: TrashItem[] }>("/api/trash", {}); }
    blockHistory(blockId: string) { return this.post<{ history: HistoryItem[] }>("/api/block/history", { blockId }); }
    restoreBlock(changeId: number) { return this.post<{ ok: boolean; block: BlockChange }>("/api/block/restore", { changeId }); }

    // ---------------- 附件 ----------------
    /** 问服务端缺哪些附件（只传缺的，省流量） */
    assetCheck(paths: string[]): Promise<{ missing: string[]; total: number }> {
        return this.post("/api/asset/check", { paths }, 30000);
    }

    /** 上传一个附件（multipart，不能用 post 的 JSON 通道） */
    async assetUpload(path: string, blob: Blob, timeout = 120000): Promise<{ ok: boolean; size: number }> {
        const url = this.serverUrl + "/api/asset/upload";
        const fd = new FormData();
        fd.append("path", path);
        fd.append("file", blob, path.split("/").pop() || "asset");
        let res: Response;
        try {
            res = await fetchTimeout(url, {
                method: "POST",
                headers: { "X-Device-Key": this.deviceKey },
                body: fd,
            }, timeout);
        } catch (e: any) {
            throw new Error(humanError(e, url));
        }
        let data: any = null;
        try { data = await res.json(); } catch { /* ignore */ }
        if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
        return data;
    }

    /** 下载附件，返回二进制；服务端没有时返回 null */
    async assetDownload(path: string, timeout = 120000): Promise<Blob | null> {
        const url = this.serverUrl + "/api/asset/download?path=" + encodeURIComponent(path);
        let res: Response;
        try {
            res = await fetchTimeout(url, { method: "GET", headers: { "X-Device-Key": this.deviceKey } }, timeout);
        } catch (e: any) {
            throw new Error(humanError(e, url));
        }
        if (res.status === 404) return null;
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return await res.blob();
    }

    /** 列出服务端全部附件（新设备首次同步用） */
    assetList(): Promise<{ assets: { path: string; size: number; hash?: string; updated: number }[] }> {
        return this.post("/api/asset/list", {}, 30000);
    }

    // ---------------- WebSocket ----------------
    connect() {
        if (!this.serverUrl || !this.deviceKey) return;
        this.closed = false;
        const wsUrl = this.serverUrl.replace(/^http/, "ws")
            + "/api/ws?device_key=" + encodeURIComponent(this.deviceKey);
        try {
            this.ws = new WebSocket(wsUrl);
        } catch (e: any) {
            this.log("error", "WebSocket 创建失败: " + (e && e.message));
            this.scheduleReconnect();
            return;
        }
        this.ws.onopen = () => {
            this.alive = true;
            if (this.hooks.onStatus) this.hooks.onStatus(true);
            this.log("info", "长连接已建立");
            // 连上先补拉断线期间错过的变更（用持久化的游标，不会重放全部历史）
            this.pull(this.lastRev).then(r => {
                this.setRev(r.latest || 0);
                (r.changes || []).forEach(c => this.hooks.onRemoteChange && this.hooks.onRemoteChange(c));
            }).catch(e => this.log("warn", "补拉失败: " + e.message));
            // 心跳：防止中间设备（反代/移动网络）把空闲连接掐掉
            if (this.pingTimer) clearInterval(this.pingTimer);
            this.pingTimer = setInterval(() => {
                try { this.ws && this.ws.readyState === 1 && this.ws.send("ping"); } catch { /* ignore */ }
            }, 25000);
        };
        this.ws.onmessage = (ev) => {
            let msg: any = null;
            try { msg = JSON.parse(ev.data); } catch { return; }
            if (!msg) return;
            if (msg.type === "remote_change" && msg.data) {
                const c = msg.data as BlockChange;
                this.setRev(c.rev || 0);
                if (this.hooks.onRemoteChange) this.hooks.onRemoteChange(c);
            } else if (msg.type === "reload") {
                if (this.hooks.onReload) this.hooks.onReload(msg.reason || "");
            } else if (msg.type === "asset_added" || msg.type === "asset_removed") {
                if (this.hooks.onAssetEvent) this.hooks.onAssetEvent(msg.type, msg.data || msg.paths);
            } else if (msg.type === "hello") {
                this.setRev(msg.latest || 0);
            }
        };
        this.ws.onclose = () => {
            this.alive = false;
            if (this.pingTimer) { clearInterval(this.pingTimer); this.pingTimer = null; }
            if (this.hooks.onStatus) this.hooks.onStatus(false);
            if (!this.closed) this.scheduleReconnect();
        };
        this.ws.onerror = () => { this.alive = false; };
    }

    private scheduleReconnect() {
        if (this.reconnectTimer || this.closed) return;
        this.reconnectTimer = setTimeout(() => {
            this.reconnectTimer = null;
            this.connect();
        }, 5000);
    }

    disconnect() {
        this.closed = true;
        if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null; }
        if (this.pingTimer) { clearInterval(this.pingTimer); this.pingTimer = null; }
        if (this.ws) { this.ws.onclose = null; try { this.ws.close(); } catch { /* ignore */ } this.ws = null; }
        this.alive = false;
        if (this.hooks.onStatus) this.hooks.onStatus(false);
    }
}
