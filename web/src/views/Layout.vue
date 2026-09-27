<template>
  <el-container style="height: 100%">
    <el-aside width="200px" style="background:#001529">
      <div style="color:#fff;font-weight:600;padding:18px 16px;font-size:16px">NAS Agent</div>
      <el-menu :default-active="$route.path" router background-color="#001529" text-color="#a6adb4"
               active-text-color="#fff" style="border-right:none">
        <el-menu-item index="/servers">服务器管理</el-menu-item>
        <el-menu-item index="/files">文件浏览</el-menu-item>
        <el-menu-item index="/transfers">转存任务</el-menu-item>
        <el-menu-item index="/bot">微信机器人</el-menu-item>
        <el-menu-item index="/users">用户管理</el-menu-item>
      </el-menu>
    </el-aside>
    <el-container>
      <el-header style="display:flex;align-items:center;justify-content:space-between;border-bottom:1px solid #eee">
        <span style="font-size:15px;color:#333">{{ $route.meta.title || '' }}</span>
        <el-dropdown @command="onCommand">
          <span style="cursor:pointer">{{ username }} <el-icon><arrow-down /></el-icon></span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item command="logout">退出登录</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </el-header>
      <el-main style="background:#f5f7fa"><router-view /></el-main>
    </el-container>
  </el-container>
</template>

<script setup>
import { ArrowDown } from '@element-plus/icons-vue'
import { useRouter } from 'vue-router'
import { logout } from '../api'
import { ref } from 'vue'

const router = useRouter()
const username = ref(localStorage.getItem('username') || 'admin')

async function onCommand(cmd) {
  if (cmd === 'logout') {
    try { await logout() } catch (e) { /* 忽略 */ }
    localStorage.removeItem('token')
    router.push('/login')
  }
}
</script>
