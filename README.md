# NAS Agent

NAS Agent 是一个连接远端文件服务器的 Web 管理工具：在 Web 界面上管理 FTP / SMB / NFS / WebDAV 服务器，并浏览、上传、下载远端文件。Agent 作为**客户端**主动连接远端文件服务器。

- 后端：Go 1.20 + Gin + SQLite（modernc.org/sqlite 纯 Go 驱动）
- 四种协议客户端全部使用 **Go 标准库手写**（不依赖第三方协议库）
- 前端：Vue 3 + Vite + Element Plus，构建产物通过 `go:embed` 打包进单个可执行文件
- 所有后端接口统一挂载在 `/api` 前缀下，根路径 `/` 为前端 SPA（手写 NoRoute 兜底，支持 history 路由）

## 快速开始

```powershell
# 构建前端（首次或前端变更后）
cd web
npm install
npm run build
cd ..

# 编译并运行
go build -o nas-agent.exe ./cmd/nas-agent
.\nas-agent.exe
```

浏览器访问 http://localhost:8080 ，默认账号 `admin / admin123`（见 `configs/application.yaml`，首次启动自动播种）。

开发模式下前端可单独运行：`cd web; npm run dev`（Vite 已配置 `/api` 代理到 8080）。

## 功能

- 登录认证（内存 token，Bearer 头；下载链接支持 `?token=` 直连；token 失效时后端返回 HTTP 401，前端自动清除本地凭证并跳回登录页）
- 服务器管理：新增 / 编辑 / 删除 / 连接测试，支持 FTP、SMB、NFS、WebDAV
- 文件浏览：列目录、双击进入、面包屑导航
- 文件操作：上传、下载、新建文件夹、删除、重命名
- 转存任务（独立菜单）：填写下载地址 → 下载到本地暂存目录（默认系统临时目录，可指定本机绝对路径）→ 下载完成后再转存到所选文件服务器上的指定存储目录；转存成功自动清理本地暂存文件，失败时保留任务记录、进度与本地文件以便重试。任务异步执行、支持进度查询，任务记录持久化到数据库（`transfer_task` 表），进程重启后仍可查看历史任务并重试；页面按「进行中 / 已完成 / 全部」三个 Tab 分类展示
  - 下载源支持 HTTP/HTTPS 直链、FTP 直链、m3u8（HLS）流媒体
  - m3u8：支持 master/media 播放列表与相对路径分片，下载全部分片后在本地用纯 Go 转封装为 MP4 再转存
  - 目标目录通过级联选择器逐层选择（懒加载，可搜索），留空表示目标服务器根目录
  - 存储目录支持从「近期使用」中快速选择：按登录用户 + 目标服务器分别记录最近 10 个用过的目录（持久化到 `transfer_recent_dir` 表）
  - 支持自定义目标文件名（留空用源文件名；m3u8 自动补 `.mp4` 后缀）
  - 失败任务可重试：可选「从头开始」（清理本地暂存文件重新下载）或「从失败步骤开始」（复用已下载的文件/分片，直链下载走 HTTP Range 续传，m3u8 跳过已完成分片）
  - 任务可暂停（中止传输，保留进度与本地暂存文件）、「开始」继续（从暂停时所处步骤续跑：下载走 Range 续传、m3u8 跳过已完成分片、上传则用本地文件重新上传），也可删除（运行中的先中止，本地暂存文件与任务记录一并清理）；暂停状态跨进程重启保留，重启后仍可继续
- 用户管理（仅管理员）：新增 / 删除用户，至少保留一个管理员
- 微信机器人（听音乐）：微信扫码登录后用指令点播 NAS 音乐目录中的歌曲；支持精确/模糊搜索交互菜单、本地没有时聚合网络搜索并经夸克网盘转存下载、自动回存音乐目录（详见下文「微信机器人（听音乐）」章节）

## 协议实现要点（均为客户端）

| 协议 | 位置 | 说明 |
| ---- | ---- | ---- |
| FTP | `internal/app/protocol/ftp` | RFC 959，被动模式（PASV），MLSD/LIST 解析，二进制传输 |
| SMB | `internal/app/protocol/smb` | SMB 2.0.2/2.1，手写 NTLMv2（MD4/HMAC-MD5）+ SPNEGO，Negotiate→SessionSetup→TreeConnect→Create/Read/Write/QueryDirectory/SetInfo |
| NFS | `internal/app/protocol/nfs` | NFSv3 over TCP，手写 XDR/ONC RPC，portmapper 发现 mountd，AUTH_SYS（uid/gid=0），文件句柄缓存 |
| WebDAV | `internal/app/protocol/webdav` | RFC 4918，PROPFIND/GET/PUT/MKCOL/DELETE/MOVE，Basic Auth |

每次文件操作按“新建连接 → 操作 → 关闭”的方式执行，不做常驻连接池。

## 目录结构

```
cmd/nas-agent/        入口：配置加载、路由注册、SPA 静态服务兜底
configs/              application.yaml
internal/app/
  common/             config / consts / rsp / util(zap)
  controller/         auth / server / file / user 控制器
  dao/                SQLite 访问（remote_server、user）
  middleware/         登录态与管理员鉴权
  model/              数据模型
  protocol/           spec(统一接口) + ftp/smb/nfs/webdav 客户端 + 工厂
  routes/             /api 路由注册
  service/            业务服务
web/                  Vue3 前端工程（构建产物 dist 由 go:embed 打包）
```

## 配置

`configs/application.yaml`：

```yaml
server:
  port: 8080
  mode: release
storage:
  root: ./data/storage
  database: ./data/nas-agent.db
  downloadDir: ""           # 下载转存任务的本地暂存目录，留空使用系统临时目录
log:
  level: info
admin:
  username: admin
  password: admin123
```

## 微信机器人（听音乐）

在 Web 端「微信机器人」页面完成配置后，即可在微信中通过指令点播 NAS 音乐目录中的歌曲；本地没有的歌曲会自动从网络搜索、经夸克网盘转存下载，并在播放后回存到音乐目录。

### 前置配置（Web 页面）

1. **音乐目录**：选择一台已配置的文件服务器与目录，递归扫描其中的音频文件（`.mp3/.flac/.wav/.m4a/.aac/.ogg/.wma/.ape`）。文件名按「歌名 - 歌手」解析（支持半角 `-` 与全角 `－/–/—` 横线）。
2. **微信机器人**：扫码登录后自动开始消息轮询；凭据持久化，**服务重启后自动恢复轮询**。
3. **夸克网盘**：扫码登录（推荐）或手动粘贴整段 Cookie。仅「网络搜索 → 网盘下载」链路需要，纯本地播放不依赖。

### 微信指令

| 指令 | 行为 |
| ---- | ---- |
| `听音乐` / `听歌` | 从音乐目录随机播放一首 |
| `听音乐 歌名` | 在音乐目录中按歌名搜索 |
| `听音乐 歌名 - 歌手` | 按歌名 + 歌手匹配 |
| `听音乐 歌手 - 歌名` | 同上，歌手与歌名顺序可互换 |

### 本地匹配与交互菜单

- **唯一精确匹配**（歌名完全一致，忽略大小写）→ 直接播放。
- **同名多首**（同一歌名有多个版本）→ 进入交互菜单。
- **只有模糊匹配**（如搜「月光」仅命中「白月光与朱砂痣」）→ 进入交互菜单，**不会再直接播放模糊结果**。
- 本地完全没有 → 直接发起网络搜索。

菜单格式（10 分钟内有效）：

```
本地找到以下相关歌曲，回复序号播放，回复 0 进行网络搜索：
1. 播放 月光 - 歌手A
2. 播放 月光 - 歌手B
0. 进入网络搜索
```

- 回复 `1/2…` 播放对应本地歌曲；序号无效时菜单保留，可重新选择。
- 回复 `0` 用原关键词进入网络搜索。

### 网络搜索与网盘下载

本地菜单回复 `0`（或本地无任何匹配）时，聚合两个来源一次性返回编号列表：

- `1–10`：全盘搜结果，标注音质（如「夸克MP3」「夸克FLAC」，优先无损）。
- `11–20`：歌曲宝结果（其下载源同样是夸克网盘分享链接）。
- 任一来源失败不影响另一来源；两个来源都为空才回复未找到。

回复序号后的处理链路：

1. 解析对应歌曲的夸克网盘分享链接（自动识别提取码；分享为文件夹时递归查找音频文件，音质优先 FLAC > WAV > APE > M4A > MP3）；
2. 转存到自己的夸克网盘 → 获取下载地址 → 下载；
3. 发送歌曲文件到微信；
4. **后台上传到 NAS 音乐目录**，下次点播同一首歌即可直接本地播放；
5. 云端转存副本下载完成后自动删除，不占用网盘容量。

> 网络搜索要求先在页面绑定夸克网盘；Cookie 失效时状态卡片会提示，重新扫码即可。

### 相关数据表

- `music_config`：音乐目录（服务器 + 目录）
- `bot_config`：微信机器人凭据与运行状态
- `quark_config`：夸克网盘 Cookie 与绑定状态
- `music_play_log`：播放/发送日志

> 机器人为登录态长轮询，**同一时间只能运行一个实例**，多实例会争抢同一机器人的消息（详见部署章节「避免多实例冲突」）。

## 部署到 Linux 服务器（systemd）

以下为在内网 CentOS 7（x86_64）服务器上的部署过程，其他 Linux 发行版同理。下文中 `<服务器IP>` 为目标服务器地址，请替换为实际 IP。

### 1. 部署前检查

```bash
uname -m            # 确认架构，如 x86_64
cat /etc/os-release # 确认系统版本
ss -tlnp | awk '{print $4}' | sort -u   # 列出已监听端口，挑选空闲端口
```

目标机的 8080 往往已被占用，本文示例选用 **8090**（部署前确认空闲）：

```bash
ss -tln | grep -E ':(8090|8091) ' || echo '8090/8091 FREE'
```

> 程序与配置统一放 `/opt/nas-agent/`，systemd 单元可参照服务器上已有的同类 Go 服务（单二进制 + `WorkingDirectory` + `Restart=on-failure`）。

### 2. 交叉编译

前端已通过 `go:embed` 内嵌，只需编译单个二进制。在开发机项目根目录：

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"
go build -o nas-agent-linux-amd64 ./cmd/nas-agent
Remove-Item Env:\GOOS,Env:\GOARCH
```

### 3. 准备配置与数据库

复制 `configs/application.yaml` 并把端口改为选定的空闲端口：

```yaml
server:
  port: 8090                # 避开已被占用的 8080
  mode: "release"
storage:
  root: "./data/storage"
  database: "./data/nas-agent.db"
  downloadDir: ""
log:
  level: "info"
admin:
  username: "admin"
  password: "admin123"
```

> 配置路径固定为相对工作目录的 `configs/application.yaml`（见 `cmd/nas-agent/main.go`），因此服务的 `WorkingDirectory` 必须是安装目录。

数据库运行在 **WAL 模式**，直接热拷贝会丢数据。**先停掉正在运行的实例**，再把三个文件一起拷出：

```powershell
Get-Process nas-agent | Stop-Process -Force   # Windows；Linux 用 systemctl stop
Start-Sleep -Seconds 2
# 三个文件缺一不可：主库 + WAL 增量 + 共享内存索引
Copy-Item .\data\nas-agent.db, .\data\nas-agent.db-wal, .\data\nas-agent.db-shm <打包目录>\
```

### 4. 上传文件

目标机创建目录：

```bash
mkdir -p /opt/nas-agent/configs /opt/nas-agent/data/storage
```

通过 SFTP 上传 5 个文件（可用任意 SFTP 工具）：

| 本地文件 | 远程路径 |
| ---- | ---- |
| `nas-agent-linux-amd64` | `/opt/nas-agent/nas-agent` |
| 修改端口后的 `application.yaml` | `/opt/nas-agent/configs/application.yaml` |
| `nas-agent.db` | `/opt/nas-agent/data/nas-agent.db` |
| `nas-agent.db-wal` | `/opt/nas-agent/data/nas-agent.db-wal` |
| `nas-agent.db-shm` | `/opt/nas-agent/data/nas-agent.db-shm` |

### 5. 创建 systemd 服务并启动

```bash
chmod +x /opt/nas-agent/nas-agent
chmod 600 /opt/nas-agent/configs/application.yaml

cat > /etc/systemd/system/nas-agent.service <<'EOF'
[Unit]
Description=NAS Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/nas-agent
ExecStart=/opt/nas-agent/nas-agent
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable nas-agent     # 开机自启
systemctl start nas-agent
systemctl status nas-agent --no-pager -l
```

启动成功的日志标志：`NAS Agent 启动于 http://localhost:8090`，且登录态机器人会打印「自动恢复……消息轮询」「……开始轮询」。

### 6. 放行防火墙

firewalld 环境需永久放行端口（若用 ufw / 云安全组，对应放行即可）：

```bash
firewall-cmd --permanent --add-port=8090/tcp
firewall-cmd --reload
```

### 7. 验证

```bash
ss -tlnp | grep 8090                                                  # 确认监听
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/       # 期望 200
curl -X POST http://127.0.0.1:8090/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}'                     # 期望 code:200 + token
```

浏览器访问 `http://<服务器IP>:8090` 登录，确认服务器配置、音乐目录、微信机器人、夸克网盘等数据均在。

### 8. 避免多实例冲突

数据库中的微信机器人为登录态，任何实例启动都会**自动恢复消息轮询**。切勿让开发机与服务器同时运行，否则会争抢同一机器人的消息，导致回复重复或遗漏。确认以服务器为唯一实例后，停掉开发机服务：

```powershell
Get-Process nas-agent | Stop-Process -Force
Get-Process nas-agent                                 # 应无输出
Get-NetTCPConnection -LocalPort 8080 -State Listen    # 应无监听
```

### 9. 日常运维与版本更新

```bash
systemctl status nas-agent      # 状态
systemctl restart nas-agent     # 重启
systemctl stop nas-agent        # 停止（同时停止微信轮询）
journalctl -u nas-agent -f      # 实时日志
```

更新二进制：重新交叉编译 → 上传覆盖 `/opt/nas-agent/nas-agent`（需保留服务器数据时**不要覆盖** `data/`）→ `chmod +x` 后 `systemctl restart nas-agent`。

部署清单：

- [ ] 检查目标机端口占用，选定空闲端口（示例 8090）
- [ ] `GOOS=linux GOARCH=amd64` 交叉编译
- [ ] 生成目标端口的 `configs/application.yaml`
- [ ] **停掉旧实例**后拷贝 db + wal + shm 三件套
- [ ] 上传二进制 / 配置 / 数据库到 `/opt/nas-agent/`
- [ ] 创建并 `enable` + `start` `nas-agent.service`
- [ ] 防火墙 / 安全组放行端口
- [ ] 本机与远程 HTTP、登录接口验证
- [ ] 停掉开发机实例，避免微信机器人多实例轮询

## 转存任务接口

| 方法 | 路径 | 说明 |
| ---- | ---- | ---- |
| POST | `/api/transfer-tasks` | 创建转存任务，body：`{sourceUrl, targetServerId, targetDir, localDir, targetFileName}`（`targetDir` 默认 `/`，`localDir` 留空用默认暂存目录，`targetFileName` 留空用源文件名） |
| GET | `/api/transfer-tasks` | 任务列表（新的在前，最多返回 100 条；运行中的任务取内存实时状态，历史任务从数据库读取） |
| GET | `/api/transfer-tasks/:taskId` | 单个任务状态与进度 |
| POST | `/api/transfer-tasks/:taskId/retry` | 重试失败任务，body：`{mode}`，`restart`=从头开始，`resume`=从失败步骤开始 |
| POST | `/api/transfer-tasks/:taskId/pause` | 暂停排队中/进行中的任务：中止传输，保留进度与本地暂存文件 |
| POST | `/api/transfer-tasks/:taskId/start` | 开始（继续）已暂停的任务：从暂停时所处步骤续跑 |
| DELETE | `/api/transfer-tasks/:taskId` | 删除任务：运行中的先中止，本地暂存文件与任务记录一并清理 |
| GET | `/api/transfer-recent-dirs` | 当前登录用户在某目标服务器下近期使用的存储目录，query：`serverId`；每个用户在每个服务器下最多保留 10 个，创建转存任务时自动记录 |

执行流程：

- 直链：打开下载地址（http/https 走 GET；ftp 复用内置 FTP 客户端 RETR）→ 下载到本地暂存目录 → 连接目标服务器（必要时逐级创建存储目录）→ 上传 → 删除本地暂存文件
- m3u8：拉取播放列表（master 则取其第一个变体流）→ 逐个下载 TS 分片 → 纯 Go 转封装为 MP4 → 上传 → 清理分片目录与暂存文件

失败处理与重试：

- 失败的任务不会删除，会保留失败原因、所处步骤（`failedStep`：download / merge / upload）与当前进度；已下载的本地文件与分片同样保留
- 重试 `mode=resume`：从 `failedStep` 继续——失败在转存则直接用本地文件重新上传；失败在 m3u8 合并则复用已下载分片重新合并；失败在下载则复用已下载的部分（http/https 用 `Range` 续传，FTP 不支持续传会重新下载）
- 重试 `mode=restart`：清理本地暂存文件与分片目录，完全重新下载
- 进程重启时仍在进行中的任务无法继续执行，会被标记为失败（说明为「服务重启导致任务中断，可重试」）；本地暂存文件与分片保留，重试规则同上
- 已暂停的任务不受进程重启影响，状态保留，重启后仍可「开始」继续

进度：直链下载/转存各占 50%；m3u8 分片下载占 50%、合并占 5%、转存占 45%。并发上限 3。

近期使用的存储目录：

- 每次创建转存任务时，会把规范化后的 `targetDir` 记入 `transfer_recent_dir` 表，与登录用户 ID、目标服务器 ID 一起存储
- 同一用户 + 同一目标服务器下最多保留 10 个目录，按最近使用排序，超出部分自动删除；重复使用同一目录只更新其位置，不会产生重复记录
- 新建转存任务时，「存储目录」左侧的「近期使用」下拉可快速选中，选中后会回填到右侧目录级联选择器

## 注意事项

- 依赖下载请使用国内代理：`$env:GOPROXY="https://goproxy.cn,direct"; $env:GOSUMDB="off"`
- **m3u8 转 MP4 为纯 Go 实现，无外部依赖**（仅做流复制 remux，不重新编码）；支持 H.264/H.265 视频与 AAC 音频，其他编码任务会以明确的错误信息失败
- 转存任务的本地暂存目录可指定本机绝对路径，需保证运行 nas-agent 的账号对该目录有写权限
- m3u8 任务会把全部分片下载到本地，临时占用空间约等于视频体积，请预留足够的本地磁盘
- 失败任务会保留本地暂存文件与分片目录（文件名以任务 ID 开头）以便重试，确认不再需要时请手动清理
- NFS 客户端要求服务端开启 NFSv3 + MOUNT 服务（portmap 111 可达）
- SMB 客户端面向 SMB2（2.0.2/2.1）且未启用签名的共享场景
