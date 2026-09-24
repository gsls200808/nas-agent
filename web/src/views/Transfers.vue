<template>
  <el-card>
    <div style="margin-bottom:12px;display:flex;gap:8px;align-items:center">
      <el-button type="primary" @click="openCreate">新建转存任务</el-button>
      <el-button @click="refresh" :loading="loading">刷新</el-button>
      <span style="color:#909399;font-size:12px">
        支持 HTTP / FTP 直链下载及 m3u8 流媒体（下载全部分片合并为 MP4 后转存）
      </span>
    </div>

    <el-tabs v-model="activeTab">
      <el-tab-pane :label="`进行中 (${counts.active})`" name="active" />
      <el-tab-pane :label="`已完成 (${counts.done})`" name="done" />
      <el-tab-pane :label="`全部 (${counts.all})`" name="all" />
    </el-tabs>

    <el-table :data="filteredTasks" v-loading="loading" stripe :empty-text="emptyText">
      <el-table-column label="类型" width="70">
        <template #default="{ row }">
          <el-tag size="small" :type="row.sourceType === 'm3u8' ? 'warning' : 'info'">{{ row.sourceType || 'http' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="文件" min-width="160" show-overflow-tooltip>
        <template #default="{ row }">{{ row.fileName || baseName(row.sourceUrl) }}</template>
      </el-table-column>
      <el-table-column label="下载地址" min-width="240" show-overflow-tooltip>
        <template #default="{ row }"><a :href="row.sourceUrl" target="_blank">{{ row.sourceUrl }}</a></template>
      </el-table-column>
      <el-table-column label="转存到" min-width="220" show-overflow-tooltip>
        <template #default="{ row }">{{ row.targetServerName }}：{{ row.targetPath || row.targetDir }}</template>
      </el-table-column>
      <el-table-column label="进度" width="200">
        <template #default="{ row }">
          <el-progress :percentage="row.progress"
                       :status="row.status === 'failed' ? 'exception' : (row.status === 'success' ? 'success' : '')" />
          <span style="font-size:12px;color:#909399">{{ phaseText(row) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }">
          <el-tooltip v-if="row.status === 'failed'" :content="row.error" placement="top">
            <el-tag type="danger" size="small">失败</el-tag>
          </el-tooltip>
          <el-tag v-else :type="statusTag(row.status)" size="small">{{ statusText(row.status) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="创建时间" width="160">
        <template #default="{ row }">{{ fmtTime(row.createdAt) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="110" fixed="right">
        <template #default="{ row }">
          <el-dropdown v-if="row.status === 'failed'" trigger="click" @command="(cmd) => doRetry(row, cmd)">
            <el-button type="primary" link size="small">
              重试<el-icon><ArrowDown /></el-icon>
            </el-button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="restart">从头开始</el-dropdown-item>
                <el-dropdown-item command="resume" :disabled="!row.failedStep">
                  从失败步骤开始{{ row.failedStep ? `（${stepText(row.failedStep)}）` : '' }}
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
          <span v-else style="color:#c0c4cc">-</span>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="createVisible" title="新建转存任务" width="650px">
      <el-form label-width="100px">
        <el-form-item label="下载地址">
          <el-input v-model="form.sourceUrl" type="textarea" :rows="2"
                    placeholder="支持 http / https / ftp 直链及 m3u8 地址，如 http://example.com/a.zip 或 https://example.com/video.m3u8" />
        </el-form-item>
        <el-form-item label="目标服务器">
          <el-select v-model="form.targetServerId" placeholder="选择文件服务器" style="width:100%" @change="onServerChange">
            <el-option v-for="s in servers" :key="s.id"
                       :label="`${s.name} (${s.protocol.toUpperCase()} ${s.host})`" :value="s.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="存储目录">
          <div style="display:flex;gap:8px;width:100%">
            <el-select
              v-model="recentDir"
              placeholder="近期使用"
              clearable
              :disabled="!form.targetServerId"
              no-data-text="暂无近期目录"
              style="width:200px"
              @change="onPickRecentDir"
            >
              <el-option v-for="d in recentDirs" :key="d" :label="d" :value="d" />
            </el-select>
            <el-cascader
              v-model="cascaderValue"
              :props="cascaderProps"
              :key="cascaderKey"
              placeholder="逐级选择目标目录"
              clearable
              filterable
              style="flex:1"
              @change="onCascaderChange"
            />
          </div>
        </el-form-item>
        <el-form-item label="目标文件名">
          <el-input v-model="form.targetFileName" placeholder="留空使用源文件名；m3u8 自动加 .mp4 后缀">
            <template #append>
              <span style="font-size:12px;color:#909399">.mp4</span>
            </template>
          </el-input>
        </el-form-item>
        <el-form-item label="本地暂存目录">
          <el-input v-model="form.localDir" placeholder="留空使用默认临时目录，也可填写本机绝对路径" />
        </el-form-item>
        <el-form-item label="">
          <span style="color:#909399;font-size:12px">
            流程：下载到本地暂存目录（m3u8 会下载全部分片并合并为 MP4）→ 转存到目标存储目录；成功后自动删除本地暂存文件，失败时保留
          </span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">开始转存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref, shallowRef } from 'vue'
import { ElMessage } from 'element-plus'
import { ArrowDown } from '@element-plus/icons-vue'
import { listServers, createTransfer, listTransferTasks, listFiles, retryTransferTask, listRecentDirs } from '../api'

const servers = ref([])
const tasks = ref([])
const loading = ref(false)

// 进行中：排队中/执行中；已完成：成功或失败（均不再执行，失败的可重试）
const activeTab = ref('active')
const isActive = (t) => t.status === 'pending' || t.status === 'running'
const counts = computed(() => ({
  active: tasks.value.filter(isActive).length,
  done: tasks.value.filter((t) => !isActive(t)).length,
  all: tasks.value.length
}))
const filteredTasks = computed(() => {
  if (activeTab.value === 'active') return tasks.value.filter(isActive)
  if (activeTab.value === 'done') return tasks.value.filter((t) => !isActive(t))
  return tasks.value
})
const emptyText = computed(() => (
  { active: '暂无进行中的任务', done: '暂无已完成的任务', all: '暂无转存任务' }[activeTab.value]
))

const createVisible = ref(false)
const submitting = ref(false)
const form = ref({ sourceUrl: '', targetServerId: null, targetDir: '/', localDir: '', targetFileName: '' })

// 级联选择器
const cascaderValue = ref([])
const cascaderKey = ref(0) // 切换服务器时强制重建
const cascaderProps = shallowRef({
  lazy: true,
  checkStrictly: true, // 允许选择任意层级
  lazyLoad: async (node, resolve) => {
    if (!form.value.targetServerId) {
      resolve([])
      return
    }
    const dir = node.level === 0 ? '/' : node.data.value
    try {
      const items = (await listFiles(form.value.targetServerId, dir)) || []
      const dirs = items
        .filter(i => i.isDir)
        .map(i => ({
          label: i.name,
          value: dir === '/' ? '/' + i.name : dir + '/' + i.name,
        }))
      resolve(dirs)
    } catch (e) {
      resolve([])
    }
  }
})

let timer = null

async function refresh() {
  loading.value = true
  try {
    tasks.value = (await listTransferTasks()) || []
  } finally {
    loading.value = false
  }
}

async function poll() {
  try {
    await refresh()
  } catch (e) { /* 轮询失败忽略 */ }
  const active = tasks.value.some((t) => t.status === 'pending' || t.status === 'running')
  timer = setTimeout(poll, active ? 1000 : 5000)
}

function startPolling() {
  stopPolling()
  poll()
}

function stopPolling() {
  if (timer) {
    clearTimeout(timer)
    timer = null
  }
}

function onServerChange() {
  cascaderValue.value = []
  form.value.targetDir = '/'
  cascaderKey.value++ // 强制重建级联选择器
  loadRecentDirs()
}

function onCascaderChange(val) {
  recentDir.value = '' // 手动选择后不再显示近期目录的选中态
  if (Array.isArray(val) && val.length > 0) {
    form.value.targetDir = val[val.length - 1]
  } else {
    form.value.targetDir = '/'
  }
}

// 近期使用的存储目录：与当前登录用户 + 目标服务器关联，由后端在创建任务时记录
const recentDirs = ref([])
const recentDir = ref('')

async function loadRecentDirs() {
  recentDir.value = ''
  if (!form.value.targetServerId) {
    recentDirs.value = []
    return
  }
  try {
    recentDirs.value = (await listRecentDirs(form.value.targetServerId)) || []
  } catch (e) {
    recentDirs.value = []
  }
}

function onPickRecentDir(dir) {
  if (!dir) {
    form.value.targetDir = '/'
    cascaderValue.value = []
    return
  }
  form.value.targetDir = dir
  cascaderValue.value = pathToCascaderValue(dir)
}

// /a/b/c → ['/a', '/a/b', '/a/b/c']，用于把目录回填到级联选择器
function pathToCascaderValue(p) {
  const segs = (p || '').split('/').filter(Boolean)
  return segs.map((_, i) => '/' + segs.slice(0, i + 1).join('/'))
}

function openCreate() {
  form.value = { sourceUrl: '', targetServerId: servers.value[0]?.id || null, targetDir: '/', localDir: '', targetFileName: '' }
  cascaderValue.value = []
  cascaderKey.value++
  createVisible.value = true
  loadRecentDirs()
}

async function submit() {
  const sourceUrl = form.value.sourceUrl.trim()
  if (!sourceUrl) {
    ElMessage.warning('请填写下载地址')
    return
  }
  if (!form.value.targetServerId) {
    ElMessage.warning('请选择目标服务器')
    return
  }
  submitting.value = true
  try {
    await createTransfer({
      sourceUrl,
      targetServerId: form.value.targetServerId,
      targetDir: form.value.targetDir || '/',
      localDir: form.value.localDir.trim(),
      targetFileName: form.value.targetFileName.trim()
    })
    createVisible.value = false
    ElMessage.success('转存任务已创建')
    startPolling()
  } finally {
    submitting.value = false
  }
}

function baseName(url) {
  return (url || '').split('?')[0].split('/').filter(Boolean).pop() || ''
}

// 重试：restart 从头开始（清理本地暂存文件重新下载）；resume 从失败步骤开始（复用已下载的文件）
async function doRetry(row, mode) {
  try {
    await retryTransferTask(row.id, mode)
    ElMessage.success(mode === 'resume' ? '已从失败步骤继续' : '已从头开始重试')
    startPolling()
  } catch (e) { /* 失败提示由 request 拦截器统一处理 */ }
}

function stepText(step) {
  return { download: '下载', merge: '合并', upload: '转存' }[step] || step
}

function phaseText(row) {
  if (row.status === 'pending') return '排队中'
  if (row.status === 'success') return '已完成，本地暂存文件已清理'
  if (row.status === 'failed') {
    return (row.failedStep ? `失败于${stepText(row.failedStep)}步骤：` : '失败：') + (row.error || '')
  }
  if (row.phase === 'merging') {
    return row.sourceType === 'm3u8' ? '合并 MP4 中…' : '处理中…'
  }
  if (row.phase === 'transferring') {
    if (row.size > 0) return `转存中 ${fmtSize(row.uploaded)} / ${fmtSize(row.size)}`
    return '转存中…'
  }
  // downloading
  if (row.sourceType === 'm3u8') {
    if (row.segments > 0) return `下载分片 ${row.downloadedSegs}/${row.segments}`
    return '下载中…'
  }
  if (row.size > 0) return `下载中 ${fmtSize(row.downloaded)} / ${fmtSize(row.size)}`
  return '下载中…'
}

function statusText(s) {
  return { pending: '排队中', running: '进行中', success: '成功', failed: '失败' }[s] || s
}

function statusTag(s) {
  return { pending: 'info', running: 'warning', success: 'success', failed: 'danger' }[s] || 'info'
}

function fmtSize(n) {
  if (n == null || n < 0) return '-'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ }
  return n.toFixed(i === 0 ? 0 : 1) + ' ' + units[i]
}

function fmtTime(t) {
  if (!t) return '-'
  const d = new Date(t)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

onMounted(async () => {
  servers.value = (await listServers()) || []
  startPolling()
})

onUnmounted(stopPolling)
</script>
