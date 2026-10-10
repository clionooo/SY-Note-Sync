/**
 * SY Note Sync - 主入口
 *
 * 工作原理（中心化同步，参考 Fast Note Sync 架构）：
 * 1. 通过思源事件总线 (eventBus "ws-main") 监听本地块变更（思源是块数据库，同步单元是块）
 * 2. 变更防抖 5 秒后批量推送到 NAS 上的同步服务
 * 3. 通过 WebSocket 长连接接收服务端下发的远端块变更，调用思源 API 写入本地
 * 4. 服务端检测到两台设备改了同一块 => 插件弹窗让用户选择版本
 *
 * 兼容性：桌面端 / Docker 网页端 / iOS APP（使用公开 API，不改内核）
 */
import {
    Plugin,
    Dialog,
    showMessage,
    type ICommand,
} from "siyuan";
import "./index.css";
import { SyncClient, BlockChange, ConflictInfo } from "./syncClient";

/** 待推送的本地变更队列元素 */
interface PendingOp {
    blockId: string;
    docId: string;
    op: "insert" | "update" | "delete";
    markdown: string;
    parentId: string;
    baseRev: number;
}

/** 本地记录的块版本表：blockId -> rev（用于冲突检测的 baseRev） */
interface BlockRevMap {
    [blockId: string]: number;
}

export default class SYNoteSync extends Plugin {
    private client: SyncClient | null = null;

    // ---- 配置项（保存在思源插件存储中）----
    private cfg = {
        serverUrl: "",    // 同步服务地址
        deviceKey: "",    // 设备密钥
        autoSync: true,   // 自动同步开关
        paused: false,    // 手动暂停
        adminToken: "",   // 管理令牌（仅注册设备时用一次）
        deviceName: "",   // 设备名称（注册时用）
    };

    private pending = new Map<string, PendingOp>(); // 待推送队列（blockId 去重，只留最新）
    private pushTimer: any = null;                  // 防抖计时器
    private blockRevs: BlockRevMap = {};            // 本地已知的块版本
    private applyingRemote = false;                 // 正在写入远端变更时，抑制本地变更上报（防回环）

    async onload() {
        // 读取持久化配置
        Object.assign(this.cfg, await this.loadData("config") || {});
        Object.assign(this.blockRevs, await this.loadData("blockRevs") || {});

        // 注册命令：立即同步（快捷键 ⌥⌘S）
        this.addCommand({
            langKey: "syncNow",
            hotkey: "⌥⌘S",
            callback: () => this.syncNow(),
        } as ICommand);

        // 注册顶栏图标（点击 = 立即同步；移动端显示在更多菜单）
        this.addTopBar({
            icon: "iconRefresh",
            title: "SY Note Sync 同步",
            position: "right",
            callback: () => this.syncNow(),
        });

        // 监听思源内核 WebSocket 事件总线：块变更事务都在这里下发
        // （基于公开 API，不修改内核）
        this.eventBus.on("ws-main", this.onMainEvent);
    }

    async onLayoutReady() {
        // 配置完整才启动同步连接
        if (this.cfg.serverUrl && this.cfg.deviceKey) {
            this.initClient();
        } else {
            console.log("SY Note Sync: 尚未配置服务器或设备密钥，请在插件设置中完成配置");
        }
    }

    async onunload() {
        this.eventBus.off("ws-main", this.onMainEvent);
        if (this.pushTimer) clearTimeout(this.pushTimer);
        if (this.client) this.client.disconnect();
        // 持久化块版本表
        await this.saveData("blockRevs", this.blockRevs);
    }

    // ================= 思源事件监听 =================

    /**
     * 思源内核 ws-main 事件处理。
     * 块变更以 cmd="transactions" 下发，doOperations 数组里是每一次块操作：
     *   action: "insert" | "update" | "delete"
     *   data:   块的 markdown 内容
     *   id / parentID / previousID: 块与父子关系
     * 这就是"监听思源官方 API 块变更事件"的公开途径。
     */
    private onMainEvent = (event: { detail?: any }) => {
        const data = event.detail;
        if (!data || data.cmd !== "transactions") return;
        // 正在应用远端变更时不上报，避免无限回环
        if (this.applyingRemote) return;
        // 暂停或未配置时不收集
        if (this.cfg.paused || !this.client) return;

        const transactions = data.data || [];
        for (const tx of transactions) {
            for (const op of tx.doOperations || []) {
                if (!op.id || op.id.length < 22) continue; // 过滤非法块 id
                const action = op.action as string;
                if (!["insert", "update", "delete"].includes(action)) continue;
                // 同一块的连续操作只保留最后一条（去重，减少推送量）
                this.pending.set(op.id, {
                    blockId: op.id,
                    docId: op.parentID || "",   // 插入场景 parentID 即文档/父块
                    op: action as PendingOp["op"],
                    markdown: op.data || "",
                    parentId: op.parentID || op.previousID || "",
                    baseRev: this.blockRevs[op.id] || 0,
                });
            }
        }
        if (this.pending.size > 0) this.schedulePush();
    };

    // ================= 推送逻辑 =================

    /** 自动模式：检测到改动后延时 5 秒推送（需求 2.3） */
    private schedulePush() {
        if (!this.cfg.autoSync || this.cfg.paused) return;
        if (this.pushTimer) clearTimeout(this.pushTimer);
        this.pushTimer = setTimeout(() => this.syncNow(), 5000);
    }

    /** 立即同步：把队列里的变更批量推给服务端 */
    private async syncNow() {
        if (!this.client) { showMessage("SY Note Sync：请先在插件设置中配置服务器与设备密钥", 3000, "error"); return; }
        if (this.pending.size === 0) { showMessage("SY Note Sync：没有待同步的变更", 2000); return; }

        const changes: BlockChange[] = Array.from(this.pending.values());
        this.pending.clear();
        try {
            const res = await this.client.push(changes);
            showMessage(`SY Note Sync：已推送 ${res.applied} 条变更`, 2000);
            // 冲突弹窗（需求 2.5）
            if (res.conflicts && res.conflicts.length > 0) {
                for (const conflict of res.conflicts) this.showConflictDialog(conflict);
            }
        } catch (e: any) {
            showMessage("SY Note Sync：推送失败 - " + e.message, 4000, "error");
            // 失败的变更放回队列，等下次重试
            for (const c of changes) this.pending.set(c.blockId, c as PendingOp);
        }
    }

    // ================= 远端变更应用 =================

    private initClient() {
        this.client?.disconnect();
        this.client = new SyncClient(
            this.cfg.serverUrl,
            this.cfg.deviceKey,
            (c) => this.applyRemoteChange(c),
            (online) => {
                console.log("SY Note Sync: 服务连接状态 =>", online ? "在线" : "离线");
            },
        );
        this.client.connect();
    }

    /**
     * 把远端块变更写入本地思源（调用思源公开 API）
     * - update: /api/block/updateBlock
     * - delete: /api/block/deleteBlock
     * - insert: /api/block/insertBlock（挂在父块下）
     */
    private async applyRemoteChange(c: BlockChange) {
        // 跳过自己刚推出去的变更（服务端不会下发给自己，保险起见再挡一层）
        if (this.pending.has(c.blockId)) return;
        this.applyingRemote = true;
        try {
            // 思源 API 与插件同源，直接走相对路径 /api/（桌面端/Docker/iOS 均适用）
            if (c.op === "update") {
                await fetch("/api/block/updateBlock", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ id: c.blockId, dataType: "markdown", data: c.markdown }),
                });
            } else if (c.op === "insert") {
                // 插入块：挂到父块下；父块不存在时（文档还没同步过来）暂跳过，
                // 由"首次全量同步"按钮兜底补齐
                await fetch("/api/block/insertBlock", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({
                        dataType: "markdown",
                        data: c.markdown,
                        parentID: c.parentId || c.docId,
                    }),
                });
            } else if (c.op === "delete") {
                await fetch("/api/block/deleteBlock", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ id: c.blockId }),
                });
            }
            // 记录该块最新版本（后续推送时作 baseRev 参与冲突检测）
            if (c.rev) this.blockRevs[c.blockId] = c.rev;
        } catch (e) {
            console.error("SY Note Sync: 应用远端变更失败", c, e);
        } finally {
            // 稍后恢复上报（等内核事务事件发完，防止把远端变更又推回服务端）
            setTimeout(() => { this.applyingRemote = false; }, 500);
        }
    }

    // ================= 冲突弹窗 =================

    /** 冲突弹窗：并列展示两个版本，用户点击选择保留哪个（需求 2.5 / 1.4） */
    private showConflictDialog(conflict: ConflictInfo) {
        const esc = (s: string) => (s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
        const html = `
<div class="b3-dialog__content synconflict">
  <p>块 <code>${esc(conflict.blockId)}</code> 在多台设备上被修改，请选择要保留的版本：</p>
  <div class="sc-versions">
    <div class="sc-ver">
      <h4>版本 A（其他设备）</h4>
      <pre>${esc(conflict.versionA.markdown)}</pre>
      <button class="b3-button b3-button--outline" id="scA">保留版本 A</button>
    </div>
    <div class="sc-ver">
      <h4>版本 B（本设备）</h4>
      <pre>${esc(conflict.versionB.markdown)}</pre>
      <button class="b3-button b3-button--outline" id="scB">保留版本 B</button>
    </div>
  </div>
</div>`;
        const dlg = new Dialog({
            title: "SY Note Sync · 检测到同步冲突",
            content: html,
            width: "640px",
        });
        const choose = async (changeId: number) => {
            try {
                await this.client!.resolveConflict(conflict.conflictId, changeId);
                showMessage("冲突已解决", 2000);
                dlg.destroy();
            } catch (e: any) {
                showMessage("解决冲突失败：" + e.message, 3000, "error");
            }
        };
        dlg.element.querySelector("#scA")?.addEventListener("click", () => choose(conflict.versionA.rev || 0));
        dlg.element.querySelector("#scB")?.addEventListener("click", () => choose(conflict.versionB.rev || 0));
    }

    // ================= 设置面板 =================

    /**
     * 插件设置页（思源公开 API：openSetting 自定义 dialog）。
     * 说明：思源"账号与同步"页由内核渲染，公开 API 无法替换其原生 UI；
     * 本面板提供等价能力：服务器地址 / 设备密钥 / 立即同步 / 自动同步 / 暂停。
     */
    openSetting(): void {
        const esc = (s: string) => (s || "").replace(/"/g, "&quot;");
        const html = `
<div class="b3-dialog__content" style="padding:16px">
  <h3>SY Note Sync 同步设置</h3>
  <div class="sc-help">
    <p><b>三步完成连接（每台设备只需操作一次）：</b></p>
    <p>① 填「同步服务地址」和「管理令牌」→ 点「注册新设备」：插件会向 NAS 上的同步服务登记本机（如"我的iPhone"），服务器现场生成一把<b>设备密钥</b>自动填入下方输入框 —— 注册就是"领钥匙"，一台设备领一次。</p>
    <p>② 点「保存配置」：插件与服务器建立长连接，此后本地改动 5 秒自动推送，远端改动实时同步过来。</p>
    <p>③ 新设备/内容不全时，点「首次全量同步」把服务器上的全部笔记拉回本机。</p>
    <p><b>两个"令牌"别混淆：</b><br>
    · <b>管理令牌</b> = 你自己定的管理员密码（部署容器时的 SYNCSRV_ADMIN_TOKEN），仅用于注册设备和看 Web 后台，日常同步用不到；<br>
    · <b>设备密钥</b> = 每台设备自己的通行证，注册时服务器自动发放，无需手填、无需记忆。换新手机/电脑再接同步时：填同样地址和管理令牌，设备名称换个新名字，再点一次注册即可。</p>
  </div>
  <label>同步服务地址（内网 IP:端口 或 https 域名）</label>
  <input id="scUrl" class="b3-text-field" style="width:100%" placeholder="如 http://192.168.1.10:58282 或 https://sync.example.com" value="${esc(this.cfg.serverUrl)}">
  <label>设备名称（注册时用，起个方便认的名字，如：我的iPhone）</label>
  <input id="scDevName" class="b3-text-field" style="width:100%" placeholder="如 我的iPhone" value="${esc(this.cfg.deviceName || "本机")}">
  <label>管理令牌（部署服务端时你自己设置的管理员密码，仅注册新设备时需要）</label>
  <input id="scAdmin" class="b3-text-field" style="width:100%" type="password" value="${esc(this.cfg.adminToken)}">
  <label>设备密钥（点"注册新设备"后自动填入，无需手填；换设备重装时可粘贴旧密钥）</label>
  <input id="scKey" class="b3-text-field" style="width:100%" type="password" value="${esc(this.cfg.deviceKey)}">
  <div style="margin-top:12px;display:flex;flex-wrap:wrap;gap:8px">
    <button class="b3-button b3-button--outline" id="scReg">① 注册新设备</button>
    <button class="b3-button b3-button--outline" id="scSave">② 保存配置</button>
    <button class="b3-button b3-button--outline" id="scNow">③ 立即同步</button>
    <button class="b3-button b3-button--outline" id="scAuto">自动同步：${this.cfg.autoSync ? "开" : "关"}</button>
    <button class="b3-button b3-button--outline" id="scPause">${this.cfg.paused ? "已暂停（点击恢复）" : "暂停同步"}</button>
    <button class="b3-button b3-button--outline" id="scFull">首次全量同步</button>
  </div>
</div>`;
        const dlg = new Dialog({ title: "SY Note Sync 设置", content: html, width: "560px" });
        const $ = (id: string) => dlg.element.querySelector(id) as HTMLInputElement;

        // 注册新设备：调服务端 /api/register，成功后自动填密钥
        dlg.element.querySelector("#scReg")?.addEventListener("click", async () => {
            const url = $("#scUrl").value.trim();
            const name = $("#scDevName").value.trim() || "未命名设备";
            const token = $("#scAdmin").value.trim();
            if (!url || !token) { showMessage("请先填写服务地址和管理令牌", 3000, "error"); return; }
            try {
                const r = await SyncClient.register(url, token, name);
                $("#scKey").value = r.deviceKey;
                showMessage(`设备"${name}"注册成功！密钥已自动填入，请点"保存配置"`, 4000);
            } catch (e: any) {
                showMessage("注册失败：" + e.message, 4000, "error");
            }
        });

        // 保存配置并（重）连接
        dlg.element.querySelector("#scSave")?.addEventListener("click", async () => {
            this.cfg.serverUrl = $("#scUrl").value.trim();
            this.cfg.deviceKey = $("#scKey").value.trim();
            this.cfg.adminToken = $("#scAdmin").value.trim();
            this.cfg.deviceName = $("#scDevName").value.trim();
            await this.saveData("config", this.cfg);
            if (this.cfg.serverUrl && this.cfg.deviceKey) this.initClient();
            showMessage("配置已保存", 2000);
        });

        dlg.element.querySelector("#scNow")?.addEventListener("click", () => { this.syncNow(); });

        // 自动同步开关
        dlg.element.querySelector("#scAuto")?.addEventListener("click", async (e) => {
            this.cfg.autoSync = !this.cfg.autoSync;
            (e.target as HTMLElement).textContent = "自动同步：" + (this.cfg.autoSync ? "开" : "关");
            await this.saveData("config", this.cfg);
        });

        // 暂停/恢复同步
        dlg.element.querySelector("#scPause")?.addEventListener("click", async (e) => {
            this.cfg.paused = !this.cfg.paused;
            (e.target as HTMLElement).textContent = this.cfg.paused ? "已暂停（点击恢复）" : "暂停同步";
            if (this.cfg.paused) { this.client?.disconnect(); } else { this.initClient(); }
            await this.saveData("config", this.cfg);
        });

        // 首次全量同步：从服务端拉全部块状态写回本地（新设备初始化）
        dlg.element.querySelector("#scFull")?.addEventListener("click", async () => {
            if (!this.client) { showMessage("请先填写并保存服务器配置", 3000, "error"); return; }
            try {
                const r = await this.client.full();
                let ok = 0;
                for (const b of (r.blocks || [])) {
                    if (b.op === "delete") continue;
                    await this.applyRemoteChange({ ...b });
                    ok++;
                }
                showMessage(`全量同步完成，共写入 ${ok} 个块`, 4000);
            } catch (e: any) {
                showMessage("全量同步失败：" + e.message, 4000, "error");
            }
        });
    }
}
