# SY Note Sync

对标 [Fast Note Sync](https://github.com/haierkeys/obsidian-fast-note-sync) 的**思源笔记中心化同步方案**：
Go 后端同步服务（Docker 容器，部署在 NAS）+ 思源笔记前端插件。
所有思源实例（iOS APP / Windows / Docker 网页端）只连接中心化同步服务，客户端之间互不访问。

## 仓库结构

```
├── server/                 # Go 后端同步服务
│   ├── main.go             #   入口 + 路由
│   ├── store.go            #   SQLite 存储层（设备/变更日志/块状态/冲突/快照）
│   ├── hub.go              #   WebSocket 枢纽（在线设备管理、远端变更广播）
│   ├── handlers.go         #   HTTP API（注册/推送/拉取/快照/冲突/后台）
│   ├── web/admin.html      #   内嵌 Web 管理后台
│   └── Dockerfile
├── plugin/                 # 思源笔记插件（集市规范，公开 API，兼容 iOS/桌面/Docker）
│   ├── src/index.ts        #   插件主逻辑（事件监听/推送/应用远端变更/冲突弹窗/设置面板）
│   ├── src/syncClient.ts   #   同步服务通信客户端（HTTP + WebSocket）
│   └── ...
├── docker-compose.yml      # 一键部署
└── docs/DEPLOY.md          # 完整部署/测试/故障排查文档
```

## 核心特性

- **块级同步**：同步单元是思源块（Block ID），基于块变更事件，不做文件遍历/md5 比对
- **多设备管理**：每设备独立密钥鉴权，Web 后台查看在线状态
- **冲突处理**：两设备改同一块 → 双版本保存 → 插件弹窗选择保留版本
- **快照与回滚**：任意时点创建快照，回滚返回全量块状态由插件写回本地
- **Web 管理后台**：`/admin` 查看设备/同步日志/冲突/快照，创建快照
- **部署简单**：单二进制 / Docker 镜像 ~20MB，SQLite 持久化，支持 HTTPS 反代

## 快速开始

```bash
# 1. 部署服务端（NAS）
docker compose up -d --build
# 管理后台 http://NAS:58282/admin

# 2. 构建插件
cd plugin && pnpm install && pnpm build

# 3. 安装插件到思源工作空间 data/plugins/sy-note-sync/ 并启用
# 4. 插件设置 → 填服务地址 + 管理令牌 → 注册新设备 → 保存 → 首次全量同步
```

详细步骤见 [docs/DEPLOY.md](docs/DEPLOY.md)。

## License

MIT
