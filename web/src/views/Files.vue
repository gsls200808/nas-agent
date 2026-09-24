<template>
  <el-card>
    <div style="margin-bottom:12px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">
      <el-select v-model="serverId" placeholder="选择服务器" style="width:220px" @change="enter('/')">
        <el-option v-for="s in servers" :key="s.id" :label="`${s.name} (${s.protocol.toUpperCase()} ${s.host})`" :value="s.id" />
      </el-select>
      <el-breadcrumb separator="/">
        <el-breadcrumb-item><a @click.prevent="enter('/')" href="#">根目录</a></el-breadcrumb-item>
        <el-breadcrumb-item v-for="seg in crumbs" :key="seg.path">
          <a @click.prevent="enter(seg.path)" href="#">{{ seg.name }}</a>
        </el-breadcrumb-item>
      </el-breadcrumb>
      <div style="flex:1"></div>
      <el-upload :action="uploadAction" :headers="uploadHeaders" :show-file-list="false"
                 :on-success="onUploaded" :on-error="onUploadError">
        <el-button type="primary" :disabled="!serverId">上传文件</el-button>
      </el-upload>
      <el-button :disabled="!serverId" @click="onMkdir">新建文件夹</el-button>
      <el-button :disabled="!serverId" @click="load">刷新</el-button>
    </div>

    <el-table :data="files" v-loading="loading" stripe @row-dblclick="onOpen">
      <el-table-column label="名称" min-width="260">
        <template #default="{ row }">
          <span style="cursor:pointer">
            <el-icon style="vertical-align:-2px;margin-right:6px;color:#e6a23c" v-if="row.isDir"><folder /></el-icon>
            <el-icon style="vertical-align:-2px;margin-right:6px;color:#409eff" v-else><document /></el-icon>
            {{ row.name }}
          </span>
        </template>
      </el-table-column>
      <el-table-column label="大小" width="120">
        <template #default="{ row }">{{ row.isDir ? '-' : fmtSize(row.size) }}</template>
      </el-table-column>
      <el-table-column label="修改时间" width="180">
        <template #default="{ row }">{{ fmtTime(row.modTime) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="240" fixed="right">
        <template #default="{ row }">
          <el-button v-if="!row.isDir" size="small" type="primary" @click="onDownload(row)">下载</el-button>
          <el-button size="small" @click="onRename(row)">重命名</el-button>
          <el-popconfirm title="确认删除？" @confirm="onDelete(row)">
            <template #reference><el-button size="small" type="danger">删除</el-button></template>
          </el-popconfirm>
        </template>
      </el-table-column>
    </el-table>
  </el-card>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Folder, Document } from '@element-plus/icons-vue'
import { listServers, listFiles, mkdir, removeFile, renameFile, uploadUrl, downloadUrl } from '../api'

const servers = ref([])
const serverId = ref(null)
const dir = ref('/')
const files = ref([])
const loading = ref(false)

const crumbs = computed(() => {
  const out = []
  let cur = ''
  for (const seg of dir.value.split('/').filter(Boolean)) {
    cur += '/' + seg
    out.push({ name: seg, path: cur })
  }
  return out
})

const uploadAction = computed(() => (serverId.value ? uploadUrl(serverId.value, dir.value) : ''))
const uploadHeaders = computed(() => ({ Authorization: 'Bearer ' + (localStorage.getItem('token') || '') }))

async function load() {
  if (!serverId.value) return
  loading.value = true
  try { files.value = (await listFiles(serverId.value, dir.value)) || [] } finally { loading.value = false }
}

function enter(path) { dir.value = path; load() }

function onOpen(row) { if (row.isDir) enter(joinPath(dir.value, row.name)) }

function joinPath(base, name) {
  if (base === '/') return '/' + name
  return base + '/' + name
}

async function onMkdir() {
  const { value } = await ElMessageBox.prompt('请输入文件夹名称', '新建文件夹', { inputPattern: /\S+/, inputErrorMessage: '名称不能为空' })
  await mkdir(serverId.value, joinPath(dir.value, value.trim()))
  ElMessage.success('已创建')
  load()
}

async function onRename(row) {
  const { value } = await ElMessageBox.prompt('请输入新名称', '重命名', { inputValue: row.name, inputPattern: /\S+/, inputErrorMessage: '名称不能为空' })
  await renameFile(serverId.value, joinPath(dir.value, row.name), joinPath(dir.value, value.trim()))
  ElMessage.success('已重命名')
  load()
}

async function onDelete(row) {
  await removeFile(serverId.value, joinPath(dir.value, row.name))
  ElMessage.success('已删除')
  load()
}

function onDownload(row) {
  const token = localStorage.getItem('token') || ''
  const a = document.createElement('a')
  a.href = downloadUrl(serverId.value, joinPath(dir.value, row.name), token)
  a.download = row.name
  a.click()
}

function onUploaded() { ElMessage.success('上传成功'); load() }
function onUploadError(err) { ElMessage.error('上传失败: ' + (err?.message || '')) }

function fmtSize(n) {
  if (n == null) return '-'
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
  if (servers.value.length) { serverId.value = servers.value[0].id; load() }
})
</script>
