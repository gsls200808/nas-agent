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
// 当前登录用户在某目标服务器下近期使用的存储目录（最多 10 个）
export const listRecentDirs = (serverId) => request.get('/transfer-recent-dirs', { params: { serverId } })

// ---- 用户管理 ----
export const listUsers = () => request.get('/users')
export const createUser = (data) => request.post('/users', data)
export const deleteUser = (id) => request.delete(`/users/${id}`)
