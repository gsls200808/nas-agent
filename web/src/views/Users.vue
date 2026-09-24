<template>
  <el-card>
    <div style="margin-bottom:12px">
      <el-button type="primary" @click="dialog = true">新增用户</el-button>
      <el-button @click="load">刷新</el-button>
    </div>
    <el-table :data="list" v-loading="loading" stripe>
      <el-table-column prop="id" label="ID" width="60" />
      <el-table-column prop="username" label="用户名" min-width="140" />
      <el-table-column label="角色" width="120">
        <template #default="{ row }">
          <el-tag :type="row.isAdmin ? 'danger' : 'info'">{{ row.isAdmin ? '管理员' : '普通用户' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="createdAt" label="创建时间" min-width="170" />
      <el-table-column label="操作" width="120">
        <template #default="{ row }">
          <el-popconfirm title="确认删除该用户？" @confirm="onDelete(row)">
            <template #reference><el-button size="small" type="danger">删除</el-button></template>
          </el-popconfirm>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="dialog" title="新增用户" width="420px">
      <el-form :model="form" label-width="80px">
        <el-form-item label="用户名"><el-input v-model="form.username" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="form.password" type="password" show-password /></el-form-item>
        <el-form-item label="管理员"><el-switch v-model="form.isAdmin" /></el-form-item>
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
import { listUsers, createUser, deleteUser } from '../api'

const list = ref([])
const loading = ref(false)
const dialog = ref(false)
const saving = ref(false)
const form = reactive({ username: '', password: '', isAdmin: false })

async function load() {
  loading.value = true
  try { list.value = (await listUsers()) || [] } finally { loading.value = false }
}

async function onSave() {
  if (!form.username || !form.password) { ElMessage.warning('请填写用户名与密码'); return }
  saving.value = true
  try {
    await createUser(form)
    ElMessage.success('已创建')
    dialog.value = false
    form.username = ''; form.password = ''; form.isAdmin = false
    load()
  } finally { saving.value = false }
}

async function onDelete(row) {
  await deleteUser(row.id)
  ElMessage.success('已删除')
  load()
}

onMounted(load)
</script>
