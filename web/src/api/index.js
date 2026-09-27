import request from './request'

// ---- 认证 ----
export const login = (data) => request.post('/auth/login', data)
export const logout = () => request.post('/auth/logout')
export const profile = () => request.get('/auth/profile')

// ---- 远端服务器 ----
export const listServers = () => request.get('/servers')
export const createServer = (data) => request.post('/servers', data)
export const updateServer = (id, data) => request.put(`/servers/${id}`, data)
export const deleteServer = (id) => request.delete(`/servers/${id}`)
export const testServer = (id) => request.post(`/servers/${id}/test`)

// ---- 远端文件 ----
export const listFiles = (id, path) => request.get(`/servers/${id}/files`, { params: { path } })
export const mkdir = (id, path) => request.post(`/servers/${id}/files/mkdir`, { path })
export const removeFile = (id, path) => request.delete(`/servers/${id}/files`, { params: { path } })
export const renameFile = (id, from, to) => request.post(`/servers/${id}/files/rename`, { from, to })
export const uploadUrl = (id, path) => `/api/servers/${id}/files/upload?path=${encodeURIComponent(path)}`
export const downloadUrl = (id, path, token) =>
  `/api/servers/${id}/files/download?path=${encodeURIComponent(path)}&token=${encodeURIComponent(token)}`

// ---- 转存任务 ----
export const createTransfer = (data) => request.post('/transfer-tasks', data)
export const listTransferTasks = () => request.get('/transfer-tasks')
export const getTransferTask = (taskId) => request.get(`/transfer-tasks/${taskId}`)
// mode: 'restart' 从头开始 / 'resume' 从失败步骤开始
export const retryTransferTask = (taskId, mode) => request.post(`/transfer-tasks/${taskId}/retry`, { mode })
// 暂停排队中/进行中的任务（中止传输，保留进度与本地暂存文件）
export const pauseTransferTask = (taskId) => request.post(`/transfer-tasks/${taskId}/pause`)
// 开始（继续）已暂停的任务
export const startTransferTask = (taskId) => request.post(`/transfer-tasks/${taskId}/start`)
// 删除任务（运行中的先中止，本地暂存文件一并清理）
export const deleteTransferTask = (taskId) => request.delete(`/transfer-tasks/${taskId}`)
// 当前登录用户在某目标服务器下近期使用的存储目录（最多 10 个）
export const listRecentDirs = (serverId) => request.get('/transfer-recent-dirs', { params: { serverId } })

// ---- 用户管理 ----
export const listUsers = () => request.get('/users')
export const createUser = (data) => request.post('/users', data)
export const deleteUser = (id) => request.delete(`/users/${id}`)

// ---- 微信机器人 ----
export const getBotStatus = () => request.get('/bot/status')
export const getBotQRCode = () => request.post('/bot/login/qrcode')
export const confirmBotLogin = (data) => request.post('/bot/login/confirm', data)
export const startBot = () => request.post('/bot/start')
export const stopBot = () => request.post('/bot/stop')
export const getMusicConfig = () => request.get('/bot/music/config')
export const saveMusicConfig = (data) => request.post('/bot/music/config', data)

// ---- 夸克网盘（机器人音乐在线搜索依赖） ----
export const getQuarkStatus = () => request.get('/bot/quark/status')
export const saveQuarkCookie = (cookie) => request.post('/bot/quark/cookie', { cookie })
export const quarkLogout = () => request.post('/bot/quark/logout')
export const getQuarkQRCode = () => request.get('/bot/quark/qrcode')
export const pollQuarkQRCode = (token) => request.post('/bot/quark/qrcode/poll', { token })
