<template>
  <el-card>
    <div style="margin-bottom:12px;display:flex;gap:8px">
      <el-button type="primary" @click="openEdit()">新增服务器</el-button>
      <el-button @click="load">刷新</el-button>
    </div>
    <el-table :data="list" v-loading="loading" stripe>
      <el-table-column prop="id" label="ID" width="60" />
      <el-table-column prop="name" label="名称" min-width="120" />
      <el-table-column prop="protocol" label="协议" width="100">
        <template #default="{ row }"><el-tag>{{ row.protocol.toUpperCase() }}</el-tag></template>
      </el-table-column>
      <el-table-column prop="host" label="主机" min-width="140" />
      <el-table-column prop="port" label="端口" width="80" />
      <el-table-column prop="username" label="用户名" min-width="100" />
      <el-table-column prop="share" label="共享/路径" min-width="120" />
      <el-table-column label="操作" width="230" fixed="right">
        <template #default="{ row }">
          <el-button size="small" type="success" :loading="row._testing" @click="onTest(row)">测试</el-button>
          <el-button size="small" @click="openEdit(row)">编辑</el-button>
          <el-popconfirm title="确认删除该服务器？" @confirm="onDelete(row)">
            <template #reference>
              <el-button size="small" type="danger">删除</el-button>
            </template>
          </el-popconfirm>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="dialog" :title="form.id ? '编辑服务器' : '新增服务器'" width="520px">
      <el-form :model="form" label-width="100px">
        <el-form-item label="协议">
          <el-select v-model="form.protocol" @change="onProtoChange" style="width:100%">
            <el-option label="FTP" value="ftp" />
            <el-option label="SMB" value="smb" />
            <el-option label="NFS" value="nfs" />
            <el-option label="WebDAV" value="webdav" />
          </el-select>
        </el-form-item>
        <el-form-item label="名称"><el-input v-model="form.name" placeholder="备注名称" /></el-form-item>
        <el-form-item label="主机"><el-input v-model="form.host" placeholder="IP 或域名" /></el-form-item>
        <el-form-item label="端口"><el-input-number v-model="form.port" :min="1" :max="65535" /></el-form-item>
        <el-form-item label="用户名"><el-input v-model="form.username" placeholder="NFS 可留空" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="form.password" type="password" show-password /></el-form-item>
        <el-form-item v-if="form.protocol === 'smb'" label="共享名">
          <el-input v-model="form.share" placeholder="如 public" />
        </el-form-item>
        <el-form-item v-if="form.protocol === 'nfs'" label="导出路径">
          <el-input v-model="form.share" placeholder="如 /export/data" />
        </el-form-item>
        <el-form-item v-if="form.protocol === 'webdav'" label="基础路径">
          <el-input v-model="form.share" placeholder="如 /dav，可留空" />
        </el-form-item>
        <el-form-item v-if="form.protocol === 'ftp'" label="起始目录">
          <el-input v-model="form.share" placeholder="可留空，默认登录目录" />
        </el-form-item>
        <el-form-item v-if="form.protocol === 'smb'" label="工作组">
          <el-input v-model="form.workgroup" placeholder="默认 WORKGROUP" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialog = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="onSave">保存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup>
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listServers, createServer, updateServer, deleteServer, testServer } from '../api'

const defaults = { protocol: 'ftp' }
const portDefaults = { ftp: 21, smb: 445, nfs: 2049, webdav: 80 }

const list = ref([])
const loading = ref(false)
const dialog = ref(false)
const saving = ref(false)
const form = reactive({ id: 0, name: '', protocol: 'ftp', host: '', port: 21, username: '', password: '', share: '', workgroup: '' })

async function load() {
  loading.value = true
  try { list.value = (await listServers()) || [] } finally { loading.value = false }
}

function openEdit(row) {
  Object.assign(form, defaults, row || { port: portDefaults[form.protocol] }, { _testing: undefined })
  if (!row) { form.id = 0; form.name = ''; form.host = ''; form.username = ''; form.password = ''; form.share = ''; form.workgroup = '' }
  dialog.value = true
}

function onProtoChange() { form.port = portDefaults[form.protocol] }

async function onSave() {
  if (!form.name || !form.host) { ElMessage.warning('请填写名称与主机'); return }
  saving.value = true
  try {
    if (form.id) await updateServer(form.id, form)
    else await createServer(form)
    ElMessage.success('已保存')
    dialog.value = false
    load()
  } finally { saving.value = false }
}

async function onDelete(row) {
  await deleteServer(row.id)
  ElMessage.success('已删除')
  load()
}

async function onTest(row) {
  row._testing = true
  try {
    await testServer(row.id)
    ElMessage.success('连接成功')
  } catch (e) {
    /* 拦截器已提示 */
  } finally { row._testing = false }
}

onMounted(load)
</script>
