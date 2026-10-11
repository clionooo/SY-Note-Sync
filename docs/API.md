# REST API

服务端有两套接口：**设备侧**（插件用，`X-Device-Key` 鉴权）和**管理后台侧**（`X-Admin-Token` 或 `?token=`）。

所有响应都是 JSON。出错时返回 `{"error": "说明"}`，HTTP 状态码为 4xx/5xx。

> 跨域：所有响应都带 `Access-Control-Allow-Origin`（回显请求的 Origin），
> 并处理 `OPTIONS` 预检。插件跑在思源前端（不同源），这一条是必须的。

---

## 通用

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 无 | 返回 `ok` |
| GET | `/api/version` | 无 | 服务端版本、协议版本、最低插件版本、站点名 |

```json
{ "server": "2.0.0", "protocol": 2, "minPlugin": "2.0.0",
  "serverName": "SY Note Sync", "shareEnabled": true, "registerOpen": true }
```

---

## 设备侧

### 注册设备

```
POST /api/register
{ "adminToken": "管理令牌", "deviceName": "我的iPhone", "platform": "ios", "version": "2.0.0" }
→ { "deviceId": 1, "deviceKey": "…", "serverVersion": "2.0.0", "protocol": 2 }
```

> 服务端设置里关掉注册后（`allow_register=0`）会返回 403。

### 握手

```
POST /api/hello     (X-Device-Key)
{ "platform": "ios", "version": "2.0.0" }
→ { "serverVersion", "protocol", "minPlugin", "device", "upgradeRequired" }
```

### 推送变更

```
POST /api/push      (X-Device-Key)
{
  "changes": [{
    "blockId": "20261011000000-aaaaaaa",
    "docId":   "20261011000000-doc0001",
    "notebookId": "20261011000000-nb00001",
    "op": "insert",                  // insert | update | delete
    "content": "<div data-node-id=…>…</div>",   // 块的 DOM
    "parentId": "20261011000000-doc0001",
    "baseRev": 0,                    // 本机看到的该块版本，服务端据此判冲突
    "force": false                   // true = 以本机为准，跳过冲突判定
  }]
}
→ { "applied": 1, "conflicts": [ … ], "latest": 12 }
```

- `content` 承载的是**思源块 DOM**（带 `data-node-id`），不是 markdown。
  写回时用 `dataType=dom` 才能保住块 ID —— 同步是按块 ID 对账的。
- 兼容 v1：字段名写 `markdown` 也能识别。
- 冲突规则：服务端该块的 `rev > baseRev` **且**最后写入者不是本设备 → 判定冲突，
  两个版本都留着（存进 `conflicts`），这条变更**不落地**，等用户裁决。

### 增量拉取

```
POST /api/pull      (X-Device-Key)
{ "since": 0 }      // 本机游标
→ { "changes": [ … ], "latest": 12 }
```

只返回 `id > since` 且**不是本设备产生**的变更。

### 全量拉取

```
POST /api/full      (X-Device-Key)
{} → { "blocks": [ … ], "latest": 12 }
```

返回服务端所有未删除块的最新状态（首次同步 / 修复用）。

### 元数据上报

```
POST /api/meta      (X-Device-Key)
{ "notebooks": [{ "id", "name", "icon" }],
  "docs":      [{ "id", "notebookId", "title", "deleted" }] }
→ { "ok": true, "notebooks": 3, "docs": 12 }
```

后台的「笔记本」页就是读这里上报的数据。

### WebSocket（实时接收）

```
ws://<host>/api/ws?device_key=<设备密钥>
```

服务端下发的消息：

| type | 内容 |
|---|---|
| `hello` | `{ latest }` 当前最新版本号 |
| `remote_change` | `{ data: BlockChange }` 其他设备的变更 |
| `reload` | `{ reason }` 服务端要求重新全量同步（例如后台回滚了快照） |

连上后插件应自行调一次 `/api/pull` 用持久化游标补齐断线期间的变更。

### 快照

| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| POST | `/api/snapshot/create` | `{name}` | `{id}` |
| POST | `/api/snapshot/list` | `{}` | `{snapshots:[…]}` |
| POST | `/api/snapshot/rollback` | `{id}` | `{blocks:[…]}` |

### 冲突

| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| POST | `/api/conflict/list` | `{}` | `{conflicts:[…]}` |
| POST | `/api/conflict/resolve` | `{conflictId, chosenChangeId}` | `{ok, block}` |

`chosenChangeId` 传 `0` 表示保留服务端版本（A），非 0 表示保留挑战版本（B）。
解决后服务端会把胜出版本广播给所有在线设备。

### 回收站 / 块历史

| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| POST | `/api/trash` | `{}` | `{trash:[…]}` 每个被删块最近一条删除记录 |
| POST | `/api/block/history` | `{blockId}` | `{history:[…]}` 该块全部历史版本 |
| POST | `/api/block/restore` | `{changeId}` | `{ok, block}` 恢复后广播给其他设备 |

---

## 附件同步（设备侧）

附件用**相对路径**做键（如 `assets/2026/10/xxx.png`，相对于思源工作空间的 `data` 目录）。
文件本体存在服务端 `<数据目录>/files/<path>`，数据库只存元信息。

| 方法 | 路径 | 请求 | 说明 |
|---|---|---|---|
| POST | `/api/asset/check` | `{paths:[...]}` → `{missing:[...], total}` | 问服务端缺哪些，客户端只传缺的 |
| POST | `/api/asset/upload` | multipart：`path` + `file` | 上传单个附件，返回 `{ok,path,size,hash}` |
| GET/POST | `/api/asset/download?path=...` | — | 返回文件字节流；404 表示服务端没有 |
| POST | `/api/asset/list` | `{}` → `{assets:[{path,size,hash,updated}]}` | 新设备首次同步时列全部附件 |
| POST | `/api/asset/delete` | `{paths:[...]}` | 删除（同时删磁盘文件并广播 `asset_removed`） |

约束：

- 只接受 `assets/` 开头的路径，会挡路径穿越（`..`、绝对路径、`\0`）
- 单个附件上限 200MB，超了返回 413
- 上传成功会向其他在线设备广播 `asset_added`

管理后台侧：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/assets?limit=500` | 列出附件 + 总数/总字节 |
| GET | `/api/admin/asset/download?path=...` | 后台直接下载/预览 |
| POST | `/api/admin/asset/delete` | `{paths:[...]}` 删除 |

---

## 管理后台侧

鉴权：请求头 `X-Admin-Token: <令牌>`，或 URL 参数 `?token=<令牌>`。

### 看板

```
GET /api/admin/data?token=…
→ { stats, devices, logs, logTotal, conflicts, snapshots, shares, notebooks,
    settings, version, protocol, now }
```

一次给全首页要的数据，减少前端请求次数。

### 设备

| 方法 | 路径 | 请求 |
|---|---|---|
| GET | `/api/admin/devices` | — |
| GET | `/api/admin/device/key?id=1` | 查看某台设备的密钥 |
| POST | `/api/admin/device/update` | `{id, name, note}` |
| POST | `/api/admin/device/rotate` | `{id}` → `{ok, key}` 旧密钥立即失效 |
| POST | `/api/admin/device/delete` | `{id}` |

### 日志与块

| 方法 | 路径 | 参数 |
|---|---|---|
| GET | `/api/admin/logs` | `page&size&device&op&q&blockId` |
| GET | `/api/admin/block/history` | `blockId` |
| POST | `/api/admin/block/restore` | `{changeId}` |

### 笔记本 / 文档

| 方法 | 路径 | 参数 |
|---|---|---|
| GET | `/api/admin/notebooks` | — |
| GET | `/api/admin/docs` | `notebook` |
| GET | `/api/admin/doc/blocks` | `doc` |

### 冲突 / 快照 / 回收站

| 方法 | 路径 | 请求 |
|---|---|---|
| GET | `/api/admin/conflicts` | `all=1` 连已解决的也返回 |
| POST | `/api/admin/conflict/resolve` | `{conflictId, chosenChangeId}` |
| GET | `/api/admin/snapshots` | — |
| POST | `/api/admin/snapshot/create` | `{name, note}` |
| POST | `/api/admin/snapshot/delete` | `{id}` |
| POST | `/api/admin/snapshot/rollback` | `{id}` 整库还原并通知设备重新同步 |
| GET | `/api/admin/trash` | — |

### 分享

| 方法 | 路径 | 请求 |
|---|---|---|
| GET | `/api/admin/shares` | — |
| POST | `/api/admin/share/create` | `{title, blockId, docId, password, expiresDays}` |
| POST | `/api/admin/share/delete` | `{id}` |

公开访问：

```
GET /api/share/{token}?password=xxx   → { title, created, views, items:[{blockId,content,…}] }
GET /share/{token}                    → 分享页 HTML
```

### 设置与导出

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/settings` | 全部设置 + 打码后的管理令牌 |
| POST | `/api/admin/settings` | 裸 key→value 字典，如 `{"site_name":"…","allow_register":"0"}` |
| GET | `/api/admin/export` | 下载 JSON 备份（全部块状态 + 设备列表） |

可用设置项：

| key | 默认 | 说明 |
|---|---|---|
| `site_name` | `SY Note Sync` | 站点名（后台标题、分享页） |
| `allow_register` | `1` | 是否允许新设备注册 |
| `share_enabled` | `1` | 是否允许创建分享链接 |
| `snapshot_keep` | `10` | 自动快照保留数量（手动快照不清理） |

### 后台实时通道

```
ws://<host>/api/admin/ws?token=<管理令牌>
```

推送事件：`hello` / `device_online` / `device_added` / `changes` / `conflict`。
后台收到后按需刷新当前页。

---

## 数据表

| 表 | 作用 |
|---|---|
| `devices` | 设备（名称、密钥、平台、版本、备注、注册/最后活跃时间） |
| `changes` | 变更流水（只追加），`id` 即全局 rev，增量拉取靠它 |
| `blocks` | 每个块的最新状态（全量同步、分享页、回收站判断靠它） |
| `conflicts` | 未解决/已解决的冲突（直接存两份内容） |
| `snapshots` / `snapshot_blocks` | 快照与快照时刻的块状态 |
| `shares` | 分享链接（token、密码、访问数、有效期） |
| `assets` | 附件元信息（路径、大小、sha256、类型）；文件本体在 `<数据目录>/files/` 下 |
| `notebooks` / `docs` | 插件上报的目录结构 |
| `settings` | 服务端设置 |
