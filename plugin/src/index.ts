/**
 * SY Note Sync · 思源笔记多设备同步插件
 *
 * 工作原理（中心化同步，参考 Fast Note Sync 架构）：
 *   1. 监听思源事件总线 ws-main 的块变更（思源是块数据库，同步单元是「块」不是文件）
 *   2. 变更防抖后批量推送到 NAS 上的同步服务
 *   3. 通过 WebSocket 长连接接收远端变更，调用思源 API 写回本地
 *   4. 两端改了同一块 => 服务端保存两个版本 => 插件弹窗让你选
 *
 * 兼容：桌面端 / Docker 网页端 / iOS APP（全部只用思源公开 API，不改内核）
 *
 * v2 相比 v1 修掉的关键问题（都是会让人以为「同步坏了」的坑）：
 *   · 注册成功后设备密钥立刻落盘，不再依赖用户再点一次「保存配置」
 *   · 待推送队列持久化，重启/切后台不丢
 *   · 增量拉取游标持久化，不会每次打开思源都重放全部历史变更
 *   · delete 操作的 data 是对象，必须挡掉，否则服务端直接 400
 *   · 写回远端用 dataType=dom，保住块 ID（同步是按块 ID 对账的）
 */
import {
    Plugin,
    Dialog,
    showMessage,
    fetchPost,
} from "siyuan";
import { SyncClient, BlockChange, ConflictInfo } from "./syncClient";
import "./index.css";

const PLUGIN_VERSION = "2.0.0";

/** 插件配置（存在思源插件私有存储里） */
interface Cfg {
    serverUrl: string;
    deviceKey: string;
    deviceName: string;
    adminToken: string;
    autoSync: boolean;
    paused: boolean;
    debounceMs: number;
    excludeNotebooks: string[];
    reportMeta: boolean;
}

const DEFAULT_CFG: Cfg = {
    serverUrl: "",
    deviceKey: "",
    deviceName: "",
    adminToken: "",
    autoSync: true,
    paused: false,
    debounceMs: 3000,
    excludeNotebooks: [],
    reportMeta: true,
};

interface LogLine { t: number; level: "info" | "warn" | "error"; msg: string; }

export default class SYNoteSync extends Plugin {
    private cfg: Cfg = { ...DEFAULT_CFG };
    private client: SyncClient | null = null;
    private pending = new Map<string, BlockChange>();
    private blockRevs: Record<string, number> = {};
    private lastRev = 0;
    private logs: LogLine[] = [];
    private applyingRemote = false;
    private pushTimer: any = null;
    private metaTimer: any = null;
    private docNotebook: Record<string, string> = {};

    /** 运行状态（设置面板实时展示） */
    private st = {
        online: false,
        pending: 0,
        lastPush: 0,
        lastPull: 0,
        lastError: "",
        serverVersion: "",
        conflictCount: 0,
    };

    // ============================================================
    // 生命周期
    // ============================================================

    async onload() {
        Object.assign(this.cfg, (await this.loadData("config")) || {});
        this.cfg = { ...DEFAULT_CFG, ...this.cfg };
        Object.assign(this.blockRevs, (await this.loadData("blockRevs")) || {});
        this.lastRev = Number(await this.loadData("lastRev")) || 0;
        const savedPending = await this.loadData("pending");
        if (Array.isArray(savedPending)) {
            savedPending.forEach((c: BlockChange) => this.pending.set(c.blockId, c));
        }
        this.st.pending = this.pending.size;

        this.addCommand({
            langKey: "syncNow",
            hotkey: "⌥⌘S",
            callback: () => this.syncNow(true),
        } as any);

        this.addTopBar({
            icon: "iconRefresh",
            title: "SY Note Sync",
            position: "right",
            callback: () => this.openSetting(),
        });

        this.eventBus.on("ws-main", this.onMainEvent);
        this.log("info", `插件 v${PLUGIN_VERSION} 已加载`);
    }

    async onLayoutReady() {
        if (this.cfg.serverUrl && this.cfg.deviceKey) {
            this.initClient();
            if (this.cfg.reportMeta) this.scheduleMetaReport();
        } else {
            this.log("warn", "尚未配置：请在插件设置里填服务地址并注册设备");
        }
    }

    async onunload() {
        this.eventBus.off("ws-main", this.onMainEvent);
        if (this.pushTimer) clearTimeout(this.pushTimer);
        if (this.metaTimer) clearTimeout(this.metaTimer);
        this.client?.disconnect();
        await this.saveData("blockRevs", this.blockRevs);
        await this.persistPending();
    }

    // ============================================================
    // 配置与日志
    // ============================================================

    private async saveCfg() {
        await this.saveData("config", this.cfg);
    }

    private log(level: "info" | "warn" | "error", msg: string) {
        this.logs.push({ t: Date.now(), level, msg });
        if (this.logs.length > 200) this.logs.splice(0, this.logs.length - 200);
        if (level === "error") this.st.lastError = msg;
        if (level === "error") console.error("[sy-note-sync]", msg);
        else console.log("[sy-note-sync]", msg);
    }

    private async persistPending() {
        await this.saveData("pending", Array.from(this.pending.values()));
        this.st.pending = this.pending.size;
    }

    private platform(): string {
        // 思源没直接暴露「iOS」，用 UA 粗判即可，只用于后台展示
        const ua = navigator.userAgent || "";
        if (/iPhone|iPad|iPod/i.test(ua)) return "ios";
        if (/Android/i.test(ua)) return "android";
        if (/Electron/i.test(ua)) return "desktop";
        return "web";
    }

    // ============================================================
    // 客户端连接
    // ============================================================

    private initClient() {
        this.client?.disconnect();
        this.client = new SyncClient(
            this.cfg.serverUrl,
            this.cfg.deviceKey,
            {
                onRemoteChange: (c) => this.applyRemoteChange(c),
                onStatus: (online) => {
                    this.st.online = online;
                    if (online) {
                        this.st.lastError = "";
                        this.log("info", "已连接同步服务");
                    } else {
                        this.log("warn", "与同步服务断开，5 秒后重连");
                    }
                },
                onRev: (rev) => {
                    this.lastRev = rev;
                    void this.saveData("lastRev", rev);
                },
                onReload: (reason) => {
                    this.log("warn", "服务端要求重新全量同步: " + reason);
                    void this.fullSync(true);
                },
                onLog: (level, msg) => this.log(level, msg),
            },
            this.lastRev,
        );
        this.client.connect();
        // 上报平台/版本，顺便拿服务端版本
        this.client.hello(this.platform(), PLUGIN_VERSION).then(h => {
            this.st.serverVersion = h.serverVersion;
            if (h.upgradeRequired) {
                showMessage(`SY Note Sync：服务端要求插件 ≥ v${h.minPlugin}，请升级插件`, 6000, "error");
            }
        }).catch(e => this.log("warn", "握手失败: " + e.message));
    }

    // ============================================================
    // 本地变更 -> 上报
    // ============================================================

    private onMainEvent = (event: { detail?: any }) => {
        const data = event.detail;
        if (!data || data.cmd !== "transactions") return;
        if (this.applyingRemote) return;                 // 正在写回远端变更，别回环
        if (this.cfg.paused || !this.client) return;     // 暂停或没配好

        const transactions = data.data || [];
        for (const tx of transactions) {
            for (const op of tx.doOperations || []) {
                if (!op.id || String(op.id).length < 22) continue;
                const action = op.action as string;
                if (action !== "insert" && action !== "update" && action !== "delete") continue;

                const docId = op.parentID || op.rootID || "";
                const nbId = this.docNotebook[docId] || "";
                if (nbId && this.cfg.excludeNotebooks.includes(nbId)) continue; // 排除的笔记本不同步

                this.pending.set(op.id, {
                    blockId: op.id,
                    docId,
                    notebookId: nbId,
                    op: action as BlockChange["op"],
                    // op.data 只有 insert/update 是 DOM 字符串；
                    // delete 的 data 是对象（如 {createEmptyParagraph:false}），
                    // 原样发出去服务端 json 解析会失败（400 请求格式错误），必须挡掉。
                    content: typeof op.data === "string" ? op.data : "",
                    parentId: op.parentID || op.previousID || "",
                    baseRev: this.blockRevs[op.id] || 0,
                });
            }
        }
        if (this.pending.size > 0) {
            this.st.pending = this.pending.size;
            void this.persistPending();
            this.schedulePush();
        }
    };

    private schedulePush() {
        if (!this.cfg.autoSync || this.cfg.paused) return;
        if (this.pushTimer) clearTimeout(this.pushTimer);
        this.pushTimer = setTimeout(() => void this.syncNow(false), Math.max(1000, this.cfg.debounceMs));
    }

    /** 立即同步：把队列里的变更批量推给服务端。manual=true 时即使没变更也给个反馈 */
    async syncNow(manual: boolean) {
        if (!this.client) {
            if (manual) showMessage("SY Note Sync：请先在插件设置里完成连接配置", 4000, "error");
            return;
        }
        if (this.pending.size === 0) {
            if (manual) showMessage("SY Note Sync：没有待同步的变更", 2000);
            return;
        }
        await this.resolveMissingDocs();
        const changes = Array.from(this.pending.values());
        this.pending.clear();
        this.st.pending = 0;
        try {
            const res = await this.client.push(changes);
            this.st.lastPush = Date.now();
            this.st.lastError = "";
            await this.persistPending();
            if (manual) showMessage(`SY Note Sync：已推送 ${res.applied} 条变更`, 2500);
            this.log("info", `推送成功 ${res.applied} 条`);
            for (const c of changes) {
                if (!this.blockRevs[c.blockId] && res.latest) this.blockRevs[c.blockId] = res.latest;
            }
            if (res.conflicts && res.conflicts.length) {
                this.st.conflictCount = res.conflicts.length;
                for (const cf of res.conflicts) this.showConflictDialog(cf);
            }
        } catch (e: any) {
            // 失败的变更放回队列，下次重试
            for (const c of changes) this.pending.set(c.blockId, c);
            this.st.pending = this.pending.size;
            await this.persistPending();
            this.log("error", "推送失败: " + e.message);
            if (manual) showMessage("SY Note Sync：推送失败 - " + e.message, 6000, "error");
        }
    }

    /**
     * 补齐 docId / notebookId。
     *
     * 思源的 update / delete 事务里 parentID 通常是空的（只有 insert 才带），
     * 所以按块 ID 反查一次内核的 blocks 表，把 root_id 和 box 填上。
     * 一次 SQL 查完整批，不是每个块查一次。
     */
    private async resolveMissingDocs() {
        const need = Array.from(this.pending.values())
            .filter(c => !c.docId || !c.notebookId)
            .map(c => c.blockId);
        if (!need.length) return;
        const inList = need.slice(0, 500).map(id => `'${id.replace(/'/g, "")}'`).join(",");
        try {
            const rows = await this.sql(`SELECT id, root_id, box FROM blocks WHERE id IN (${inList})`);
            const map: Record<string, { root: string; box: string }> = {};
            for (const r of rows || []) {
                map[r.id] = { root: r.root_id || "", box: r.box || "" };
            }
            for (const c of this.pending.values()) {
                const hit = map[c.blockId];
                if (!hit) continue;
                if (!c.docId && hit.root) c.docId = hit.root;
                if (!c.notebookId && hit.box) c.notebookId = hit.box;
                if (c.docId) this.docNotebook[c.docId] = c.notebookId || this.docNotebook[c.docId] || "";
            }
        } catch (e: any) {
            this.log("warn", "补齐文档信息失败（不影响同步）: " + (e && e.message));
        }
    }

    // ============================================================
    // 远端变更 -> 写回本地
    // ============================================================
    private async applyRemoteChange(c: BlockChange) {
        if (!c || !c.blockId) return;
        if (this.pending.has(c.blockId)) return;   // 本地还有没推出去的改动，先不覆盖
        this.applyingRemote = true;
        try {
            if (c.op === "update") {
                await this.post("/api/block/updateBlock", {
                    id: c.blockId, dataType: "dom", data: c.content,
                });
            } else if (c.op === "insert") {
                await this.post("/api/block/insertBlock", {
                    dataType: "dom", data: c.content,
                    parentID: c.parentId || c.docId || "",
                });
            } else if (c.op === "delete") {
                await this.post("/api/block/deleteBlock", { id: c.blockId });
            }
            if (c.rev) this.blockRevs[c.blockId] = c.rev;
            this.st.lastPull = Date.now();
        } catch (e: any) {
            this.log("warn", `写回块 ${c.blockId} 失败: ${e && e.message}`);
        } finally {
            // 等内核把事务事件发完再恢复上报，防止把远端变更又推回去
            setTimeout(() => { this.applyingRemote = false; }, 600);
        }
    }

    /** 调思源内核 API（与插件同源，桌面/Docker/iOS 都适用） */
    private post(path: string, body: any): Promise<any> {
        return new Promise((resolve, reject) => {
            fetchPost(path, body, (res) => {
                if (res && res.code !== 0 && res.code !== undefined) {
                    reject(new Error(res.msg || ("code " + res.code)));
                } else {
                    resolve(res);
                }
            });
        });
    }

    // ============================================================
    // 全量同步
    // ============================================================

    async fullSync(silent = false) {
        if (!this.client) {
            if (!silent) showMessage("SY Note Sync：请先完成连接配置", 3000, "error");
            return;
        }
        try {
            const r = await this.client.full();
            let ok = 0, fail = 0;
            for (const b of r.blocks || []) {
                if (b.op === "delete") continue;
                try { await this.applyRemoteChange(b); ok++; } catch { fail++; }
            }
            if (r.latest) {
                this.lastRev = r.latest;
                await this.saveData("lastRev", r.latest);
            }
            this.st.lastPull = Date.now();
            this.log("info", `全量同步完成：写入 ${ok} 个块${fail ? `，失败 ${fail}` : ""}`);
            if (!silent) showMessage(`SY Note Sync：全量同步完成，写入 ${ok} 个块`, 4000);
        } catch (e: any) {
            this.log("error", "全量同步失败: " + e.message);
            if (!silent) showMessage("SY Note Sync：全量同步失败 - " + e.message, 5000, "error");
        }
    }

    // ============================================================
    // 元数据上报（笔记本 / 文档列表，后台要用）
    // ============================================================

    private scheduleMetaReport() {
        if (this.metaTimer) clearTimeout(this.metaTimer);
        this.metaTimer = setTimeout(() => void this.reportMeta(), 8000);
    }

    private sql(stmt: string): Promise<any[]> {
        return new Promise((resolve) => {
            fetchPost("/api/query/sql", { stmt }, (res) => {
                resolve(res && res.code === 0 ? (res.data || []) : []);
            });
        });
    }

    /** 取某个块的 DOM（对文档块会返回整篇文档的 DOM，含全部子块和块 ID） */
    private getBlockDOM(id: string): Promise<string> {
        return new Promise((resolve, reject) => {
            fetchPost("/api/block/getBlockDOM", { id }, (res) => {
                if (res && res.code === 0 && res.data) resolve(res.data.dom || "");
                else reject(new Error((res && res.msg) || "取 DOM 失败"));
            });
        });
    }

    /**
     * 全量上传：把本机每篇文档的完整 DOM 推给服务端。
     *
     * 为什么需要它：服务端刚部署时是空库，而插件只在「有编辑」时才推变更。
     * 不主动上传的话，另一台设备做全量同步会什么都拉不到。
     * 按文档为单位推（一篇文档一次请求），带上 force 标记跳过冲突判定。
     */
    async fullUpload() {
        if (!this.client) { showMessage("SY Note Sync：请先完成连接配置", 3000, "error"); return; }
        if (this.cfg.paused) { showMessage("已暂停同步，请先恢复再上传", 2500, "error"); return; }
        showMessage("SY Note Sync：正在全量上传本机内容，请稍候…", 4000);
        this.log("info", "开始全量上传本机内容");
        try {
            const docs = await this.sql("SELECT id, box FROM blocks WHERE type = 'd' LIMIT 3000");
            let ok = 0, fail = 0;
            for (const d of docs || []) {
                try {
                    const dom = await this.getBlockDOM(d.id);
                    if (!dom) { fail++; continue; }
                    await this.client.push([{
                        blockId: d.id,
                        docId: d.id,
                        notebookId: d.box || "",
                        op: "update",
                        content: dom,
                        force: true,
                    }]);
                    ok++;
                } catch (e: any) {
                    fail++;
                    this.log("warn", `上传文档 ${d.id} 失败: ${e && e.message}`);
                }
            }
            this.st.lastPush = Date.now();
            this.log("info", `全量上传完成：成功 ${ok} 篇${fail ? `，失败 ${fail} 篇` : ""}`);
            showMessage(`SY Note Sync：全量上传完成，${ok} 篇文档${fail ? `，失败 ${fail} 篇（见日志）` : ""}`, 5000);
        } catch (e: any) {
            this.log("error", "全量上传失败: " + e.message);
            showMessage("SY Note Sync：全量上传失败 - " + e.message, 5000, "error");
        }
    }

    async reportMeta() {
        if (!this.client || !this.cfg.reportMeta) return;
        try {
            const nbs = await this.sql("SELECT id, name, icon FROM boxes WHERE closed = 0");
            const docs = await this.sql("SELECT id, box, content FROM blocks WHERE type = 'd' LIMIT 3000");
            const notebooks = (nbs || []).map((n: any) => ({
                id: n.id, name: n.name || "", icon: n.icon || "",
            }));
            const docList = (docs || []).map((d: any) => ({
                id: d.id, notebookId: d.box || "", title: d.content || "", deleted: false,
            }));
            // 本地缓存 docId -> notebookId，推变更时才能带上笔记本信息
            for (const d of docList) this.docNotebook[d.id] = d.notebookId;
            await this.client.reportMeta({ notebooks, docs: docList });
            this.log("info", `已上报 ${notebooks.length} 个笔记本 / ${docList.length} 篇文档`);
        } catch (e: any) {
            this.log("warn", "元数据上报失败: " + (e && e.message));
        }
    }

    // ============================================================
    // 冲突弹窗
    // ============================================================

    private showConflictDialog(cf: ConflictInfo) {
        const esc = (s: string) => String(s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
        const dlg = new Dialog({
            title: "SY Note Sync · 检测到同步冲突",
            width: "680px",
            content: `<div class="b3-dialog__content synconflict">
  <p>块 <code>${esc(cf.blockId)}</code> 在两台设备上都被改过，请选择保留哪一版：</p>
  <div class="sc-versions">
    <div class="sc-ver"><h4>版本 A（服务端当前）</h4><pre>${esc(cf.versionA.content)}</pre>
      <button class="b3-button b3-button--outline" data-pick="0">保留 A</button></div>
    <div class="sc-ver"><h4>版本 B（本机刚推的）</h4><pre>${esc(cf.versionB.content)}</pre>
      <button class="b3-button b3-button--outline" data-pick="1">保留 B</button></div>
  </div>
</div>`,
        });
        dlg.element.querySelectorAll("[data-pick]").forEach(btn => {
            btn.addEventListener("click", async () => {
                const pick = Number((btn as HTMLElement).dataset.pick);
                try {
                    await this.client!.resolveConflict(cf.id, pick);
                    showMessage("冲突已解决", 2000);
                    this.st.conflictCount = Math.max(0, this.st.conflictCount - 1);
                    dlg.destroy();
                } catch (e: any) {
                    showMessage("解决冲突失败：" + e.message, 3000, "error");
                }
            });
        });
    }

    // ============================================================
    // 设置面板
    // ============================================================

    openSetting(): void {
        const esc = (s: any) => String(s == null ? "" : s)
            .replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;");
        const cfg = this.cfg;

        const html = `
<div class="b3-dialog__content sc-wrap">
  <div class="sc-tabs">
    <button class="sc-tab active" data-tab="conn">连接</button>
    <button class="sc-tab" data-tab="sync">同步</button>
    <button class="sc-tab" data-tab="dev">设备</button>
    <button class="sc-tab" data-tab="log">日志</button>
    <button class="sc-tab" data-tab="about">关于</button>
  </div>

  <div class="sc-pane" data-pane="conn">
    <div class="sc-status" id="scStatus"></div>
    <div class="sc-help">
      <p><b>三步连上（每台设备只做一次）</b></p>
      <p>① 填服务地址 → 点「测试连接」确认网络通<br>
         ② 填管理令牌 + 设备名称 → 点「注册并保存」（密钥会自动存好，不用再点别的）<br>
         ③ 新设备再点「首次全量同步」把服务器上的笔记拉回来</p>
      <p><b>两个令牌别混：</b>管理令牌 = 部署时设的管理员密码（只用于注册设备/进后台）；
         设备密钥 = 每台设备自己的通行证，注册时自动发放。</p>
    </div>
    <div class="sc-field"><label>同步服务地址</label>
      <input id="scUrl" class="b3-text-field sc-full" placeholder="http://192.168.8.8:58282 或 https://sync.example.com" value="${esc(cfg.serverUrl)}"></div>
    <div class="sc-field"><label>设备名称</label>
      <input id="scDevName" class="b3-text-field sc-full" placeholder="如 我的iPhone" value="${esc(cfg.deviceName || "本机")}"></div>
    <div class="sc-field"><label>管理令牌</label>
      <input id="scAdmin" class="b3-text-field sc-full" type="password" placeholder="部署服务端时设置的 SYNCSRV_ADMIN_TOKEN" value="${esc(cfg.adminToken)}"></div>
    <div class="sc-field"><label>设备密钥（注册后自动填入）</label>
      <input id="scKey" class="b3-text-field sc-full" type="password" value="${esc(cfg.deviceKey)}"></div>
    <div class="sc-btns">
      <button class="b3-button b3-button--outline" id="scTest">测试连接</button>
      <button class="b3-button b3-button--text" id="scReg">注册并保存</button>
      <button class="b3-button b3-button--outline" id="scSave">只保存配置</button>
    </div>
  </div>

  <div class="sc-pane sc-hide" data-pane="sync">
    <div class="sc-row"><span>自动同步</span>
      <button class="b3-button b3-button--outline" id="scAuto">${cfg.autoSync ? "已开启" : "已关闭"}</button></div>
    <div class="sc-row"><span>暂停同步</span>
      <button class="b3-button b3-button--outline" id="scPause">${cfg.paused ? "已暂停（点此恢复）" : "未暂停"}</button></div>
    <div class="sc-row"><span>上报笔记本结构</span>
      <button class="b3-button b3-button--outline" id="scMeta">${cfg.reportMeta ? "已开启" : "已关闭"}</button></div>
    <div class="sc-field"><label>防抖延迟（毫秒，改完多久自动推送）</label>
      <input id="scDeb" class="b3-text-field sc-full" type="number" value="${cfg.debounceMs}"></div>
    <div class="sc-field"><label>排除同步的笔记本（每行一个笔记本 ID，留空=全部同步）</label>
      <textarea id="scEx" class="b3-text-field sc-full" rows="3">${esc((cfg.excludeNotebooks || []).join("\n"))}</textarea></div>
    <div class="sc-btns">
      <button class="b3-button b3-button--text" id="scNow">立即同步</button>
      <button class="b3-button b3-button--outline" id="scFull">首次全量同步（拉回本机）</button>
      <button class="b3-button b3-button--outline" id="scUpload">全量上传本机内容</button>
      <button class="b3-button b3-button--outline" id="scSnap">创建服务端快照</button>
    </div>
    <div class="sc-help" style="margin-top:12px">
      <p><b>首次全量同步</b>：把服务端的块写回本机（新设备第一次用）。</p>
      <p><b>全量上传</b>：把本机每篇文档推给服务端（服务端还是空库、或本机内容更全时用）。
         以本机为准，不会弹冲突。</p>
    </div>
  </div>

  <div class="sc-pane sc-hide" data-pane="dev">
    <div class="sc-help"><p>本机设备信息与服务端版本。</p></div>
    <div class="sc-kv" id="scDevInfo"></div>
    <div class="sc-btns">
      <button class="b3-button b3-button--outline" id="scTrash">回收站 / 块历史</button>
      <button class="b3-button b3-button--outline" id="scConf">查看未解决冲突</button>
    </div>
  </div>

  <div class="sc-pane sc-hide" data-pane="log">
    <div class="sc-btns" style="margin-bottom:8px">
      <button class="b3-button b3-button--outline" id="scCopyLog">复制诊断信息</button>
      <button class="b3-button b3-button--outline" id="scClearLog">清空</button>
    </div>
    <pre class="sc-log" id="scLogBox"></pre>
  </div>

  <div class="sc-pane sc-hide" data-pane="about">
    <div class="sc-help">
      <p><b>SY Note Sync v${PLUGIN_VERSION}</b> · 思源笔记中心化同步</p>
      <p>服务端：Go + SQLite，客户端只连服务端，设备之间零直连。</p>
      <p>项目地址：github.com/clionooo/SY-Note-Sync</p>
    </div>
    <div class="sc-kv" id="scAbout"></div>
  </div>
</div>`;

        const dlg = new Dialog({ title: "SY Note Sync 设置", content: html, width: "620px" });
        const q = (id: string) => dlg.element.querySelector("#" + id) as HTMLInputElement;
        const qa = (sel: string) => dlg.element.querySelector(sel) as HTMLElement;

        // ---- 分页签 ----
        dlg.element.querySelectorAll(".sc-tab").forEach(tab => {
            tab.addEventListener("click", () => {
                dlg.element.querySelectorAll(".sc-tab").forEach(t => t.classList.remove("active"));
                tab.classList.add("active");
                const name = (tab as HTMLElement).dataset.tab;
                dlg.element.querySelectorAll(".sc-pane").forEach(p => {
                    p.classList.toggle("sc-hide", (p as HTMLElement).dataset.pane !== name);
                });
                if (name === "dev") renderDev();
                if (name === "log") renderLog();
            });
        });

        const renderStatus = () => {
            const st = this.st;
            qa("#scStatus").innerHTML = `
              <span class="sc-pill ${st.online ? "ok" : "bad"}">${st.online ? "● 已连接" : "○ 未连接"}</span>
              <span class="sc-pill">待推送 ${st.pending}</span>
              ${st.serverVersion ? `<span class="sc-pill">服务端 v${esc(st.serverVersion)}</span>` : ""}
              ${st.conflictCount ? `<span class="sc-pill bad">冲突 ${st.conflictCount}</span>` : ""}
              ${st.lastError ? `<div class="sc-err">${esc(st.lastError)}</div>` : ""}`;
        };
        const renderDev = () => {
            qa("#scDevInfo").innerHTML = `
              <div><span>设备名称</span><b>${esc(this.cfg.deviceName || "未设置")}</b></div>
              <div><span>设备密钥</span><b class="sc-mono">${esc((this.cfg.deviceKey || "").slice(0, 8))}…</b></div>
              <div><span>服务地址</span><b class="sc-mono">${esc(this.cfg.serverUrl || "未设置")}</b></div>
              <div><span>运行平台</span><b>${esc(this.platform())}</b></div>
              <div><span>增量游标</span><b class="sc-mono">${this.lastRev}</b></div>
              <div><span>最近推送</span><b>${this.st.lastPush ? new Date(this.st.lastPush).toLocaleString() : "—"}</b></div>
              <div><span>最近拉取</span><b>${this.st.lastPull ? new Date(this.st.lastPull).toLocaleString() : "—"}</b></div>`;
        };
        const renderLog = () => {
            const box = qa("#scLogBox");
            box.textContent = this.logs.slice(-120).map(l =>
                `${new Date(l.t).toLocaleTimeString()} [${l.level}] ${l.msg}`).join("\n") || "（暂无日志）";
            box.scrollTop = box.scrollHeight;
        };
        renderStatus();
        renderLog();

        // ---- 测试连接 ----
        q("#scTest").addEventListener("click", async () => {
            const url = q("#scUrl").value.trim();
            if (!url) { showMessage("请先填服务地址", 2500, "error"); return; }
            q("#scTest").disabled = true;
            try {
                const v = await SyncClient.ping(url);
                this.st.serverVersion = v.serverVersion;
                showMessage(`连接成功：${v.serverName || "SY Note Sync"} v${v.serverVersion}`, 4000);
                this.log("info", "测试连接成功: " + url);
            } catch (e: any) {
                showMessage("连接失败：" + e.message, 7000, "error");
                this.log("error", "测试连接失败: " + e.message);
            } finally {
                q("#scTest").disabled = false;
                renderStatus();
            }
        });

        // ---- 注册并保存（关键：注册成功立刻落盘，不依赖用户再点保存）----
        q("#scReg").addEventListener("click", async () => {
            const url = q("#scUrl").value.trim();
            const name = q("#scDevName").value.trim() || "未命名设备";
            const token = q("#scAdmin").value.trim();
            if (!url) { showMessage("请先填服务地址", 2500, "error"); return; }
            if (!token) { showMessage("请先填管理令牌", 2500, "error"); return; }
            q("#scReg").disabled = true;
            try {
                const r = await SyncClient.register(url, token, name, this.platform(), PLUGIN_VERSION);
                // 立刻写进配置并落盘 —— 这一步是 v1 最大的坑：
                // v1 只把密钥填进输入框，用户没再点「保存配置」就丢了
                this.cfg.serverUrl = url;
                this.cfg.deviceName = name;
                this.cfg.adminToken = token;
                this.cfg.deviceKey = r.deviceKey;
                await this.saveCfg();
                q("#scKey").value = r.deviceKey;
                this.st.serverVersion = r.serverVersion;
                this.log("info", `注册成功: ${name} (deviceId=${r.deviceId})`);
                showMessage(`注册成功！设备「${name}」已保存，正在连接…`, 4000);
                this.initClient();
                renderStatus();
            } catch (e: any) {
                showMessage("注册失败：" + e.message, 7000, "error");
                this.log("error", "注册失败: " + e.message);
            } finally {
                q("#scReg").disabled = false;
            }
        });

        // ---- 只保存配置 ----
        q("#scSave").addEventListener("click", async () => {
            this.cfg.serverUrl = q("#scUrl").value.trim();
            this.cfg.deviceName = q("#scDevName").value.trim();
            this.cfg.adminToken = q("#scAdmin").value.trim();
            this.cfg.deviceKey = q("#scKey").value.trim();
            await this.saveCfg();
            if (this.cfg.serverUrl && this.cfg.deviceKey) this.initClient();
            showMessage("配置已保存", 2000);
            renderStatus();
        });

        // ---- 同步页签 ----
        q("#scAuto").addEventListener("click", async (e) => {
            this.cfg.autoSync = !this.cfg.autoSync;
            (e.target as HTMLElement).textContent = this.cfg.autoSync ? "已开启" : "已关闭";
            await this.saveCfg();
        });
        q("#scPause").addEventListener("click", async (e) => {
            this.cfg.paused = !this.cfg.paused;
            (e.target as HTMLElement).textContent = this.cfg.paused ? "已暂停（点此恢复）" : "未暂停";
            if (this.cfg.paused) this.client?.disconnect(); else this.initClient();
            await this.saveCfg();
            renderStatus();
        });
        q("#scMeta").addEventListener("click", async (e) => {
            this.cfg.reportMeta = !this.cfg.reportMeta;
            (e.target as HTMLElement).textContent = this.cfg.reportMeta ? "已开启" : "已关闭";
            await this.saveCfg();
            if (this.cfg.reportMeta) this.scheduleMetaReport();
        });
        q("#scDeb").addEventListener("change", async () => {
            this.cfg.debounceMs = Math.max(1000, Number(q("#scDeb").value) || 3000);
            await this.saveCfg();
        });
        q("#scEx").addEventListener("change", async () => {
            this.cfg.excludeNotebooks = q("#scEx").value.split("\n").map(s => s.trim()).filter(Boolean);
            await this.saveCfg();
            showMessage("已保存排除列表", 2000);
        });
        q("#scNow").addEventListener("click", () => void this.syncNow(true));
        q("#scFull").addEventListener("click", () => void this.fullSync(false));
        q("#scUpload").addEventListener("click", () => void this.fullUpload());
        q("#scSnap").addEventListener("click", async () => {
            if (!this.client) { showMessage("请先完成连接配置", 2500, "error"); return; }
            try {
                await this.client.createSnapshot("来自 " + (this.cfg.deviceName || "本机") + " 的快照");
                showMessage("快照已创建", 2500);
            } catch (e: any) {
                showMessage("创建快照失败：" + e.message, 4000, "error");
            }
        });

        // ---- 设备页签 ----
        q("#scTrash").addEventListener("click", () => this.showTrashDialog());
        q("#scConf").addEventListener("click", async () => {
            if (!this.client) { showMessage("请先完成连接配置", 2500, "error"); return; }
            try {
                const r = await this.client.listConflicts();
                this.st.conflictCount = (r.conflicts || []).length;
                if (!r.conflicts || !r.conflicts.length) { showMessage("没有未解决的冲突", 2500); return; }
                r.conflicts.forEach(c => this.showConflictDialog(c));
            } catch (e: any) {
                showMessage("查询失败：" + e.message, 4000, "error");
            }
        });

        // ---- 日志页签 ----
        q("#scCopyLog").addEventListener("click", () => {
            const info = [
                `SY Note Sync v${PLUGIN_VERSION}`,
                `平台: ${this.platform()}`,
                `思源: ${(window as any).siyuan?.config?.system?.kernelVersion || "?"}`,
                `服务地址: ${this.cfg.serverUrl}`,
                `设备: ${this.cfg.deviceName} / key ${(this.cfg.deviceKey || "").slice(0, 8)}…`,
                `游标: ${this.lastRev} / 待推送: ${this.pending.size}`,
                `服务端: ${this.st.serverVersion}`,
                `--- 日志 ---`,
                ...this.logs.slice(-80).map(l => `${new Date(l.t).toLocaleString()} [${l.level}] ${l.msg}`),
            ].join("\n");
            navigator.clipboard?.writeText(info).then(
                () => showMessage("诊断信息已复制到剪贴板", 2500),
                () => showMessage("复制失败，请手动全选日志框内容", 3000, "error"));
        });
        q("#scClearLog").addEventListener("click", () => { this.logs = []; renderLog(); });

        // 状态定时刷新（弹窗开着时每 2 秒同步一次显示）
        const timer = setInterval(() => {
            if (!dlg.element || !document.body.contains(dlg.element)) { clearInterval(timer); return; }
            renderStatus();
        }, 2000);
    }

    // ============================================================
    // 回收站 / 块历史
    // ============================================================

    private showTrashDialog() {
        if (!this.client) { showMessage("请先完成连接配置", 3000, "error"); return; }
        const esc = (s: any) => String(s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
        const dlg = new Dialog({
            title: "SY Note Sync · 回收站与块历史",
            width: "720px",
            content: `<div class="b3-dialog__content" style="padding:12px">
  <div style="display:flex;gap:8px;margin-bottom:10px">
    <input id="scHistId" class="b3-text-field" style="flex:1" placeholder="输入块 ID 查该块的全部历史版本（可选）">
    <button class="b3-button b3-button--outline" id="scHistGo">查历史</button>
  </div>
  <div id="scTrashList" style="max-height:52vh;overflow:auto">加载中…</div>
</div>`,
        });
        const listEl = dlg.element.querySelector("#scTrashList") as HTMLElement;

        const renderTrash = async () => {
            try {
                const r = await this.client!.trash();
                const list = r.trash || [];
                listEl.innerHTML = list.length ? list.map(t => `
                  <div class="sc-trash-item">
                    <div><b>${esc(t.blockId)}</b> · 删除于 ${new Date(t.ts * 1000).toLocaleString()} · ${esc(t.deviceName || ("设备" + t.deviceId))}</div>
                    <pre>${esc((t.content || "").replace(/<[^>]+>/g, " ").slice(0, 200)) || "(无内容)"}</pre>
                    <button class="b3-button b3-button--outline" data-restore="${t.id}">恢复此块</button>
                  </div>`).join("")
                    : "回收站为空（被删除的块会永久留在这里，随时可恢复）";
            } catch (e: any) {
                listEl.textContent = "加载失败：" + e.message;
            }
        };

        listEl.addEventListener("click", async (ev) => {
            const btn = (ev.target as HTMLElement).closest("[data-restore]") as HTMLElement | null;
            if (!btn) return;
            try {
                const r = await this.client!.restoreBlock(Number(btn.dataset.restore));
                await this.applyRemoteChange(r.block);
                showMessage("已恢复，并已同步到其他设备", 2500);
                renderTrash();
            } catch (e: any) {
                showMessage("恢复失败：" + e.message, 3000, "error");
            }
        });

        dlg.element.querySelector("#scHistGo")?.addEventListener("click", async () => {
            const id = (dlg.element.querySelector("#scHistId") as HTMLInputElement).value.trim();
            if (!id) return;
            try {
                const r = await this.client!.blockHistory(id);
                const list = r.history || [];
                listEl.innerHTML = list.length ? list.map(h => `
                  <div class="sc-trash-item">
                    <div><b>${esc(h.op)}</b> · #${h.id} · ${esc(h.deviceName || ("设备" + h.deviceId))} · ${new Date(h.ts * 1000).toLocaleString()}</div>
                    <pre>${esc((h.content || "").replace(/<[^>]+>/g, " ").slice(0, 200)) || "(无内容)"}</pre>
                    <button class="b3-button b3-button--outline" data-restore="${h.id}">恢复此版本</button>
                  </div>`).join("") : "该块没有历史记录";
            } catch (e: any) {
                showMessage("查询失败：" + e.message, 3000, "error");
            }
        });

        renderTrash();
    }
}
