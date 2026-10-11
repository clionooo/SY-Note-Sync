# 部署与排障

## 一、服务端部署

### 方式 A：docker compose（推荐）

```bash
git clone https://github.com/clionooo/SY-Note-Sync.git
cd SY-Note-Sync
```

编辑 `docker-compose.yml`，把 `SYNCSRV_ADMIN_TOKEN` 改成你自己的令牌（**必须改**，默认值会打警告日志）：

```yaml
services:
  sy-note-sync:
    build: ./server
    container_name: sy-note-sync
    restart: unless-stopped
    user: "1000:1001"        # 以你的用户身份跑，数据文件属主正确、SMB 里能备份
    ports:
      - "58282:58282"
    environment:
      - SYNCSRV_ADMIN_TOKEN=换成你自己的令牌
      - TZ=Asia/Shanghai
    volumes:
      - ./data:/app/data
```

```bash
docker compose up -d --build
curl http://127.0.0.1:58282/healthz     # ok
```

### 环境变量 / 启动参数

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-addr` | `SYNCSRV_ADDR` | `:58282` | 监听地址 |
| `-db` | `SYNCSRV_DB` | `data/sync.db` | SQLite 路径 |
| `-data` | `SYNCSRV_DATA` | `data` | 数据目录 |
| `-admin-token` | `SYNCSRV_ADMIN_TOKEN` | `change-me` | 管理令牌 |
| `-base-url` | `SYNCSRV_BASE_URL` | 空 | 对外地址，拼分享链接用 |

### 数据与备份

- 全部数据都在挂载的 `data/` 目录（`sync.db` + WAL）。**备份这个目录 = 备份全部同步数据**。
- SQLite 是 WAL 模式，拷贝前建议先 `docker compose stop`，或者用后台的「导出 JSON 备份」。
- 老版本（v1）的库可以直接被 v2 打开：启动时会自动补列/改列名（`markdown` → `content`），
  日志里能看到 `[store] 已迁移 ...`。`conflicts` 表结构差异太大且 v1 里基本是空的，会被重建。

## 二、反向代理（外网 / HTTPS）

iOS 或在外网用手机时，需要把服务暴露出去。**WebSocket 必须透传**，否则收不到实时推送。

### Nginx

```nginx
location / {
    proxy_pass http://127.0.0.1:58282;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;      # WebSocket 升级
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 300s;                     # WS 长连接超时拉长
}
```

### Lucky（飞牛常用）

1. 新建反向代理规则：域名 → 后端 `127.0.0.1:58282`
2. **开启 WebSocket 支持**
3. 开启 HTTPS（Lucky 自带 ACME 申请证书）
4. 插件里地址填 `https://sync.example.com`（**不要带末尾斜杠**）

服务端本身也支持 HTTPS 反代后的 `X-Forwarded-For`（日志里会显示真实客户端 IP）。

## 三、插件安装

### 桌面 / Docker 网页端

```bash
cp -r plugin/dist/* "<思源工作空间>/data/plugins/sy-note-sync/"
```

重启思源 → 设置 → 集市 → 已下载 → 启用「笔记同步」。

启用状态记录在 `<工作空间>/data/storage/petal/petals.json`，格式是对象数组：

```json
[{ "name": "sy-note-sync", "displayName": "笔记同步", "version": "2.0.0", "enabled": true }]
```

> ⚠️ 手写这个文件时**必须是对象数组**，写成 `["sy-note-sync"]` 内核会报
> `unmarshal petals failed` 并跳过所有插件。

### 移动端安装

iOS / Android 思源没有「从文件装插件」的入口，两条路：

**路线 1（推荐）：手机连 NAS 上的工作空间**
让手机思源连接 NAS 上那个 Docker 思源的工作空间，插件装在服务端工作空间里，手机自动可用。
不用在手机上单独维护插件。

**路线 2：本地工作空间 + 文件导入**
1. 把 `dist/sy-note-sync-v2.0.0-ios.zip` 解压，得到 `sy-note-sync/` 目录
2. 用「文件」App / 爱思助手等把整个目录放进手机思源工作空间的 `data/plugins/` 下
3. 重启思源 App → 设置 → 集市 → 已下载 → 启用

## 四、设备注册流程

每台设备一次：

1. 插件设置 → 填「同步服务地址」（如 `http://192.168.8.8:58282`）
2. 点 **测试连接** —— 只验证网络与服务是否可达，不需要密钥
3. 填「管理令牌」+「设备名称」→ 点 **注册并保存**
   （v2 会在注册成功时**立即把密钥写进配置并落盘**，不需要再点别的按钮）
4. 新设备点 **首次全量同步**；如果服务端还是空库，先在内容最全的设备上点 **全量上传**

> 两个令牌别混：
> - **管理令牌** = 部署服务端时设的管理员密码（`SYNCSRV_ADMIN_TOKEN`），只用于注册设备和进后台
> - **设备密钥** = 每台设备自己的通行证，注册时服务端发放
>
> 手机端注册失败时，可以在**服务端后台 → 设备 → 密钥**里查到已注册设备的密钥，手动粘进插件。

## 五、排障清单

| 现象 | 排查步骤 |
|---|---|
| 点「测试连接」失败 | 提示里带具体原因。先确认手机/电脑和 NAS 在同一网络，浏览器能打开 `http://<NAS IP>:58282/healthz` |
| 提示「连不上服务端」 | 地址/端口写错、服务没起、被防火墙拦。注意地址**不要带末尾斜杠** |
| 提示「管理令牌错误」 | 核对 `docker-compose.yml` 的 `SYNCSRV_ADMIN_TOKEN`，改完要 `docker compose up -d` 重建容器 |
| 注册成功但同步不动 | 后台看设备是否「在线」。不在线说明 WebSocket 没连上（反代没开 WebSocket 是常见原因） |
| 收不到实时推送 | 反代开 WebSocket；或先用内网 IP 直连排除反代问题 |
| 改了笔记另一端没反应 | ① 后台「变更日志」有没有记录：有 → 推送成功，问题在接收端写回，看插件日志；② 接收端点一次「首次全量同步」兜底 |
| 新设备全量同步后没内容 | 服务端库是空的。先在内容最全的设备上点「全量上传」 |
| 块插入后位置不对 | 插入用 parentID 定位，父块还没同步过来时会跳过该条，做一次全量同步兜底 |
| 数据丢失风险 | 定期备份挂载卷 `data/`（拷贝前建议先停容器） |

## 六、开发提示

### 插件构建的两个坑

1. **思源只加载 `index.js` 和 `index.css`**，而 vite 的 cjs 产物叫 `index.cjs`、
   样式产物叫 `style.css` —— `vite.config.ts` 里的 `closeBundle` 会复制改名。
   漏了改名样式会被**静默丢弃**（面板变丑，没有任何报错）。
2. **在带 ACL 的卷上（飞牛 / 群晖）直接 `npm install` 会失败**：
   `node_modules/esbuild/bin/esbuild` 会丢掉可执行位，报 `EACCES`。
   所以 `scripts/build.sh` 把插件放到 `/tmp` 下构建再拷回来。

### 插件私有存储

- 配置：`<工作空间>/data/storage/petal/sy-note-sync/config`（**没有扩展名**，
  思源的 `loadData("config")` 读的就是这个名字）
- 游标：同目录下的 `lastRev`
- 待推送队列：`pending`
- 块版本表：`blockRevs`

### 服务端接口自测

```bash
TOK=你的管理令牌
curl http://127.0.0.1:58282/api/version
curl "http://127.0.0.1:58282/api/admin/data?token=$TOK"
```

后台「设置 → 导出 JSON 备份」可以把全部块状态导出来做离线核对。
