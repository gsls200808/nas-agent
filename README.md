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
  - m3u8：支持 master/media 播放列表与相对路径分片，下载全部分片后在本地用 ffmpeg 合并为 MP4 再转存
  - 目标目录通过级联选择器逐层选择（懒加载，可搜索），留空表示目标服务器根目录
  - 存储目录支持从「近期使用」中快速选择：按登录用户 + 目标服务器分别记录最近 10 个用过的目录（持久化到 `transfer_recent_dir` 表）
  - 支持自定义目标文件名（留空用源文件名；m3u8 自动补 `.mp4` 后缀）
  - 失败任务可重试：可选「从头开始」（清理本地暂存文件重新下载）或「从失败步骤开始」（复用已下载的文件/分片，直链下载走 HTTP Range 续传，m3u8 跳过已完成分片）
- 用户管理（仅管理员）：新增 / 删除用户，至少保留一个管理员

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

## 转存任务接口

| 方法 | 路径 | 说明 |
| ---- | ---- | ---- |
| POST | `/api/transfer-tasks` | 创建转存任务，body：`{sourceUrl, targetServerId, targetDir, localDir, targetFileName}`（`targetDir` 默认 `/`，`localDir` 留空用默认暂存目录，`targetFileName` 留空用源文件名） |
| GET | `/api/transfer-tasks` | 任务列表（新的在前，最多返回 100 条；运行中的任务取内存实时状态，历史任务从数据库读取） |
| GET | `/api/transfer-tasks/:taskId` | 单个任务状态与进度 |
| POST | `/api/transfer-tasks/:taskId/retry` | 重试失败任务，body：`{mode}`，`restart`=从头开始，`resume`=从失败步骤开始 |
| GET | `/api/transfer-recent-dirs` | 当前登录用户在某目标服务器下近期使用的存储目录，query：`serverId`；每个用户在每个服务器下最多保留 10 个，创建转存任务时自动记录 |

执行流程：

- 直链：打开下载地址（http/https 走 GET；ftp 复用内置 FTP 客户端 RETR）→ 下载到本地暂存目录 → 连接目标服务器（必要时逐级创建存储目录）→ 上传 → 删除本地暂存文件
- m3u8：拉取播放列表（master 则取其第一个变体流）→ 逐个下载 TS 分片 → 本地按序拼接 → ffmpeg remux 为 MP4 → 上传 → 清理分片目录与暂存文件

失败处理与重试：

- 失败的任务不会删除，会保留失败原因、所处步骤（`failedStep`：download / merge / upload）与当前进度；已下载的本地文件与分片同样保留
- 重试 `mode=resume`：从 `failedStep` 继续——失败在转存则直接用本地文件重新上传；失败在 m3u8 合并则复用已下载分片重新合并；失败在下载则复用已下载的部分（http/https 用 `Range` 续传，FTP 不支持续传会重新下载）
- 重试 `mode=restart`：清理本地暂存文件与分片目录，完全重新下载
- 进程重启时仍在进行中的任务无法继续执行，会被标记为失败（说明为「服务重启导致任务中断，可重试」）；本地暂存文件与分片保留，重试规则同上

进度：直链下载/转存各占 50%；m3u8 分片下载占 50%、合并占 5%、转存占 45%。并发上限 3。

近期使用的存储目录：

- 每次创建转存任务时，会把规范化后的 `targetDir` 记入 `transfer_recent_dir` 表，与登录用户 ID、目标服务器 ID 一起存储
- 同一用户 + 同一目标服务器下最多保留 10 个目录，按最近使用排序，超出部分自动删除；重复使用同一目录只更新其位置，不会产生重复记录
- 新建转存任务时，「存储目录」左侧的「近期使用」下拉可快速选中，选中后会回填到右侧目录级联选择器

## 注意事项

- 依赖下载请使用国内代理：`$env:GOPROXY="https://goproxy.cn,direct"; $env:GOSUMDB="off"`
- **m3u8 转 MP4 需要本机安装 `ffmpeg` 并加入 PATH**（仅做流复制 remux，不重新编码）；未安装时任务会以明确的错误信息失败
- 转存任务的本地暂存目录可指定本机绝对路径，需保证运行 nas-agent 的账号对该目录有写权限
- m3u8 任务会把全部分片下载到本地，临时占用空间约等于视频体积，请预留足够的本地磁盘
- 失败任务会保留本地暂存文件与分片目录（文件名以任务 ID 开头）以便重试，确认不再需要时请手动清理
- NFS 客户端要求服务端开启 NFSv3 + MOUNT 服务（portmap 111 可达）
- SMB 客户端面向 SMB2（2.0.2/2.1）且未启用签名的共享场景
