import axios from 'axios'
import { ElMessage } from 'element-plus'
import router from '../router'

// 统一 axios 实例：与 epg-front-end 相同的 code===200 约定
const request = axios.create({ baseURL: '/api', timeout: 120000 })

// 未登录 / 登录过期：清除本地凭证并跳回登录页
function redirectToLogin(msg) {
  localStorage.removeItem('token')
  localStorage.removeItem('username')
  localStorage.removeItem('isAdmin')
  ElMessage.warning(msg || '未登录或登录已过期，请重新登录')
  if (router.currentRoute.value.path !== '/login') router.push('/login')
}

request.interceptors.request.use((config) => {
  const token = localStorage.getItem('token')
  if (token) config.headers.Authorization = 'Bearer ' + token
  return config
})

request.interceptors.response.use(
  (resp) => {
    // 二进制下载流直接返回
    if (resp.config.responseType === 'blob') return resp
    const body = resp.data
    if (body.code === 200) return body.data
    if (body.code === 401) {
      redirectToLogin(body.msg)
      return Promise.reject(new Error(body.msg || '未登录'))
    }
    ElMessage.error(body.msg || '请求失败')
    return Promise.reject(new Error(body.msg || '请求失败'))
  },
  (err) => {
    // 后端未登录时返回 HTTP 401，需要跳回登录页重新登录
    if (err.response?.status === 401) {
      redirectToLogin(err.response.data?.msg)
      return Promise.reject(err)
    }
    const msg = err.response?.data?.msg || err.message || '网络错误'
    ElMessage.error(msg)
    return Promise.reject(err)
  }
)

export default request
