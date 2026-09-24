<template>
  <div style="height:100%;display:flex;align-items:center;justify-content:center;background:#eef1f6">
    <el-card style="width:360px">
      <div style="text-align:center;font-size:20px;font-weight:600;margin-bottom:18px">NAS Agent</div>
      <el-form :model="form" @keyup.enter="onLogin">
        <el-form-item>
          <el-input v-model="form.username" placeholder="用户名" />
        </el-form-item>
        <el-form-item>
          <el-input v-model="form.password" type="password" placeholder="密码" show-password />
        </el-form-item>
        <el-button type="primary" style="width:100%" :loading="loading" @click="onLogin">登 录</el-button>
      </el-form>
    </el-card>
  </div>
</template>

<script setup>
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { login } from '../api'

const router = useRouter()
const form = reactive({ username: 'admin', password: '' })
const loading = ref(false)

async function onLogin() {
  if (!form.username || !form.password) {
    ElMessage.warning('请输入用户名和密码')
    return
  }
  loading.value = true
  try {
    const data = await login(form)
    localStorage.setItem('token', data.token)
    localStorage.setItem('username', data.username)
    localStorage.setItem('isAdmin', data.isAdmin ? '1' : '0')
    router.push('/servers')
  } finally {
    loading.value = false
  }
}
</script>
