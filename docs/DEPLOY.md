# SY Note Sync 部署与测试文档

中心化思源笔记同步方案：**Go 后端同步服务（Docker）+ 思源插件**。
所有思源实例（iOS APP / Windows / Docker 网页端）只连接同步服务，互相不直连。

## 一、整体架构

```
┌─────────────┐      ┌─────────────┐      ┌─────────────┐
│  iOS 思源APP │      │ Windows思源  │      │ Docker 思源  │
│  (插件)      │      │  (插件)      │      │  (插件)      │
└──────┬──────┘      └──────┬──────┘      └──────┬──────┘
       │  HTTP 推/拉 + WebSocket 接收         │
       └──────────────┬────┴────────────────────┘
                      ▼
        ┌───────────────────────────────┐
        │   SY Note Sync 服务 (Go)      │  ← 飞牛 NAS Docker 部署
        │  设备注册/密钥鉴权             │
        │  块变更日志 + 最新块状态        │     SQLite
        │  冲突检测（双版本保存）         │
        │  快照/回滚  ·  Web 管理后台    │
        └───────────────────────────────┘
```

- 同步单元是**块（Block ID）**，不是 md 文件（思源是块数据库）
- 服务端只存插件上报的块变更，**不直接读写思源数据库**；思源读写全部由插件走思源公开 API
- 客户端之间零直连，无 P2P

## 二、后端编译与 Docker 部署（飞牛 NAS）

### 本地编译（可选，Docker 构建不需要）

```bash
cd server
CGO_ENABLED=0 go build -o syncsrv .
./syncsrv -addr :58282 -admin-token 你的令牌
```

### Docker 构建与启动

```bash
# 在仓库根目录
docker compose up -d --build
# 验证
curl http://NAS的IP:58282/healthz    # 返回 ok
```

端口：默认 `58282`，在 `docker-compose.yml` 的 `ports` 里改。
数据卷：`./data`（SQLite + 快照），备份这个目录即备份全部同步数据。
管理后台：`http://NAS的IP:58282/admin`（令牌 = `SYNCSRV_ADMIN_TOKEN`）。

## 三、Lucky 反代配置（域名 + HTTPS + WebSocket）

以 Lucky（飞牛常用反代工具）为例：

1. 添加反向代理规则：域名如 `sync.example.com` → 后端 `127.0.0.1:58282`
2. **开启 WebSocket 支持**（关键，否则插件收不到远端推送；Lucky 里一般是"启用 WebSocket"开关，开启即可，反代会自动透传 `Upgrade` 头）
3. 开启 HTTPS 证书（Lucky 自带 ACME 自动申请）；插件里地址填 `https://sync.example.com`
4. 如果用 Nginx 反代，需要加：

```nginx
location / {
    proxy_pass http://127.0.0.1:58282;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;      # WebSocket 升级
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 300s;                     # WS 长连接超时拉长
}
```

## 四、插件打包与安装

### 打包

```bash
cd plugin
pnpm install && pnpm build
# dist/ 就是插件包
```

### 安装到 Docker 思源

```bash
# 思源工作空间路径按实际情况替换
cp -r plugin/dist/* /你的思源工作空间/data/plugins/sy-note-sync/
```

重启思源 → 设置 → 集市/插件 → 启用「笔记同步」。
（也可用「加载仓库插件」功能直接挂载 dist 目录调试。）

### 安装到 iOS 思源 APP

iOS 思源与桌面端插件 API 一致。iOS 没有文件管理器直装插件的便捷入口，
推荐两种方式：
1. iOS 思源连接的是同一个 Docker 工作空间（Remote 模式）→ 插件装在服务端，iOS 自动可用；
2. 本地模式：通过 iOS「文件」App 把插件目录放进思源工作空间 `data/plugins/sy-note-sync/`（用思源内置的「打开工作空间」或第三方文件管理），然后重启启用。

## 五、设备注册与首次同步

每台设备操作一次：

1. 插件设置面板 → 填「同步服务地址」：
   - 内网：`http://192.168.x.x:58282`
   - 外网域名：`https://sync.example.com`
2. 填「管理令牌」（docker-compose 里的 `SYNCSRV_ADMIN_TOKEN`）和「设备名称」（如 `我的iPhone`）
3. 点 **① 注册新设备** → 服务端分配设备密钥，自动填入
4. 点 **② 保存配置** → 插件与服务器建立 WebSocket 长连接
5. 新设备点 **首次全量同步** → 从服务端拉取全部块状态写回本地
6. 之后正常编辑即可：本地改动 5 秒后自动推送；远端改动实时下发

## 六、测试流程

### 内网测试

1. NAS 启动服务 → Windows 思源注册为「PC」→ iOS 注册为「iPhone」
2. Windows 改一段文字 → 5 秒内 iPhone 上同内容出现 → ✅ 推送/下发正常
3. iPhone 断网 1 分钟，期间 Windows 改两处 → iPhone 恢复网络 → 断线期间的变更自动补齐（WS 重连后增量拉取）→ ✅

### 外网域名反代测试

1. 手机关 Wi-Fi 用流量，地址填 `https://sync.example.com`
2. 编辑 → 修改出现在内网 Windows 端 → ✅ HTTPS + 反代 + WS 全通

### 冲突场景测试

1. **两端同时离线改同一块**：Windows 与 iPhone 都断网，各自修改同一段文字
2. 双方恢复网络先后推送：后推的一方会收到服务端冲突响应，弹出「版本 A / 版本 B」弹窗
3. 任选一版 → 服务端把胜出版本广播给所有设备 → 所有端同内容 → ✅
4. 未解决冲突也可在 Web 后台 `/admin` 查看

### 快照与回滚测试

1. Web 后台或插件创建快照
2. 删一些内容 → 后台「回滚」→ 各端点「首次全量同步」恢复到快照状态

### 回收站与历史版本测试

1. 任意设备删除一个块 → Web 后台「回收站」出现该块（保留被删前内容和删除时间/设备）
2. 插件设置 →「回收站 / 历史」→ 点恢复 → 本块写回本机，服务端同时广播给其他设备 → 所有端内容一致
3. 后台日志里点任意块 ID → 弹窗显示该块全部历史版本 → 可恢复任意一版

## 七、常见故障排查清单

| 现象 | 排查步骤 |
|---|---|
| 注册失败"管理令牌错误" | 核对 docker-compose 的 `SYNCSRV_ADMIN_TOKEN` 与插件里填的是否一致；改过要 `docker compose up -d` 重启 |
| 推送失败/连接不上 | `curl http://NAS:58282/healthz` 先确认服务活着；检查插件地址有没有多写空格/斜杠 |
| 收不到远端实时推送 | WS 问题：反代是否开启 WebSocket；直接试 `http://IP:58282` 内网地址对比；看服务端日志 `[hub] 设备 x 已连接` |
| iOS 连不上 https | 证书是否有效（Let's Encrypt）；自签证书 iOS 会拒绝 |
| 内容推送了但另一端没变 | 看服务端 `/admin` 变更日志有没有记录；有记录说明插件端 apply 失败，看思源开发者控制台报错 |
| 新设备全量同步后块丢失 | 全量同步只还原服务端"最新块状态"；确认服务端收到过该设备的历史推送 |
| 块插入后位置不对 | insert 用 parentID 定位；父块未同步时该条跳过，做一次全量同步兜底 |
| 数据丢失风险 | 定期备份挂载卷 `./data`（SQLite WAL 模式，拷贝前建议先停容器） |

## 八、已知边界（v0.1）

- 「账号与同步」原生 UI 无法替换（思源公开 API 限制），用插件设置面板提供等价功能
- 新插入块的父块尚未同步到目标端时，该条插入会被跳过（全量同步兜底）
- 服务端不感知思源文档树结构，文档级的新建/重命名依赖块事件推导，复杂结构变化建议用全量同步兜底
