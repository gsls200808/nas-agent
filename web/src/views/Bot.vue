<template>
  <el-card>
    <template #header>
      <div style="display:flex;align-items:center;justify-content:space-between">
        <span>微信机器人</span>
        <el-tag :type="status.loggedIn ? (status.status === 'online' ? 'success' : 'warning') : 'info'">
          {{ status.loggedIn ? (status.status === 'online' ? '在线' : '离线') : '未登录' }}
        </el-tag>
      </div>
    </template>

    <!-- 登录区域 -->
    <el-card v-if="!status.loggedIn" shadow="never" style="margin-bottom:16px">
      <template #header><span>扫码登录</span></template>
      <div v-if="!qrImg" style="text-align:center;padding:20px">
        <el-button type="primary" :loading="loadingQR" @click="fetchQR">获取登录二维码</el-button>
      </div>
      <div v-else style="text-align:center;padding:10px">
        <img :src="qrImg" alt="登录二维码" style="width:260px;height:260px;border:1px solid #eee;border-radius:8px" />
        <div style="margin-top:12px;color:#666">请使用微信扫描二维码</div>
        <div v-if="confirming" style="margin-top:8px;color:#e6a23c">等待确认登录...</div>
        <div style="margin-top:12px">
          <el-button size="small" @click="resetQR">刷新二维码</el-button>
          <el-link v-if="qrUrl" :href="qrUrl" target="_blank" type="primary" :underline="false" style="margin-left:8px">二维码异常？在新窗口打开</el-link>
        </div>
      </div>
    </el-card>

    <!-- 已登录信息 -->
    <el-card v-else shadow="never" style="margin-bottom:16px">
      <template #header><span>机器人信息</span></template>
      <el-descriptions :column="2" border>
        <el-descriptions-item label="Bot ID">{{ status.botId || '-' }}</el-descriptions-item>
        <el-descriptions-item label="iLink User ID">{{ status.ilinkUserId || '-' }}</el-descriptions-item>
        <el-descriptions-item label="状态">
          <el-tag :type="status.status === 'online' ? 'success' : 'info'">{{ status.status }}</el-tag>
        </el-descriptions-item>
      </el-descriptions>
      <div style="margin-top:16px;display:flex;gap:8px">
        <el-button v-if="status.status !== 'online'" type="primary" @click="onStart">启动机器人</el-button>
        <el-button v-else type="danger" @click="onStop">停止机器人</el-button>
        <el-button @click="fetchStatus">刷新状态</el-button>
        <el-button type="warning" @click="onLogout">重新登录</el-button>
      </div>
    </el-card>

    <!-- 音乐配置 -->
    <el-card shadow="never">
      <template #header><span>音乐目录配置</span></template>
      <el-form :model="musicForm" label-width="100px" style="max-width:600px">
        <el-form-item label="目标服务器">
          <el-select v-model="musicForm.serverId" placeholder="选择NAS服务器" style="width:100%" @change="onMusicServerChange">
            <el-option v-for="s in servers" :key="s.id" :label="`${s.name} (${s.protocol.toUpperCase()} ${s.host})`" :value="s.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="音乐目录">
          <el-cascader
            v-model="musicCascaderValue"
            :props="musicCascaderProps"
            :key="musicCascaderKey"
            :disabled="!musicForm.serverId"
            placeholder="逐级选择音乐目录"
            clearable
            filterable
            style="width:100%"
            @change="onMusicCascaderChange"
          />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="savingMusic" @click="onSaveMusic">保存配置</el-button>
        </el-form-item>
      </el-form>
      <el-alert type="info" :closable="false" style="margin-top:12px">
        <p>音乐目录支持逐级选择，清空选择则使用服务器根目录「/」，机器人会在该目录下递归查找音频。</p>
        <p>在微信中向机器人发送以下指令：</p>
        <p><b>听音乐</b> — 随机播放一首音乐</p>
        <p><b>听音乐 歌名</b> — 搜索并播放指定歌曲；本地没有时自动在线搜索，回复序号即可下载播放（需绑定夸克网盘）</p>
      </el-alert>
    </el-card>

    <!-- 夸克网盘 -->
    <el-card shadow="never" style="margin-top:16px">
      <template #header>
        <div style="display:flex;align-items:center;justify-content:space-between">
          <span>夸克网盘</span>
          <el-tag :type="quarkStatus.configured ? (quarkStatus.valid ? 'success' : 'danger') : 'info'">
            {{ quarkStatus.configured ? (quarkStatus.valid ? `已绑定: ${quarkStatus.nickname || '未知'}` : 'Cookie 已失效') : '未绑定' }}
          </el-tag>
        </div>
      </template>

      <el-alert type="info" :closable="false" style="margin-bottom:16px">
        绑定夸克网盘后，微信发送「听音乐 歌名」时本地没有的歌曲会自动在线搜索，回复序号即可转存下载并发送，歌曲同时保存到音乐目录。
      </el-alert>

      <!-- 扫码登录 -->
      <div v-if="!quarkStatus.configured">
        <div v-if="!quarkQrImg" style="text-align:center;padding:10px">
          <el-button type="primary" :loading="quarkLoadingQR" @click="fetchQuarkQR">扫码登录夸克网盘</el-button>
          <el-divider>或</el-divider>
          <el-input v-model="quarkCookieInput" type="textarea" :rows="3" placeholder="粘贴 pan.quark.cn 的完整 Cookie（需整段复制）" style="max-width:600px" />
          <div style="margin-top:8px">
            <el-button :loading="quarkSavingCookie" @click="onSaveQuarkCookie">保存 Cookie</el-button>
          </div>
        </div>
        <div v-else style="text-align:center;padding:10px">
          <img :src="quarkQrImg" alt="夸克登录二维码" style="width:260px;height:260px;border:1px solid #eee;border-radius:8px" />
          <div style="margin-top:12px;color:#666">请使用夸克 APP 扫描二维码</div>
          <div style="margin-top:12px">
            <el-button size="small" @click="resetQuarkQR">刷新二维码</el-button>
          </div>
        </div>
      </div>

      <!-- 已绑定 -->
      <div v-else style="display:flex;gap:8px">
        <el-button @click="fetchQuarkStatus">刷新状态</el-button>
        <el-button type="warning" :loading="quarkLoggingOut" @click="onQuarkLogout">解绑</el-button>
      </div>
    </el-card>
  </el-card>
</template>

<script setup>
import { onMounted, onUnmounted, reactive, ref, shallowRef } from 'vue'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import {
  getBotStatus, getBotQRCode, confirmBotLogin,
  startBot, stopBot, getMusicConfig, saveMusicConfig,
  listServers, listFiles,
  getQuarkStatus, saveQuarkCookie, quarkLogout, getQuarkQRCode, pollQuarkQRCode
} from '../api'

const status = ref({ loggedIn: false, botId: '', ilinkUserId: '', status: 'offline' })
const servers = ref([])
const loadingQR = ref(false)
const qrUrl = ref('')        // 微信登录页 URL（二维码编码内容）
const qrImg = ref('')        // 本地生成的二维码图片 data URL
const qrCode = ref('')
const qrCodeImgContent = ref('')
const confirming = ref(false)

const musicForm = reactive({ serverId: 0, musicDir: '/' })
const savingMusic = ref(false)

// 音乐目录级联选择器（懒加载，只列文件夹；与转存任务页一致）
const musicCascaderValue = ref([])
const musicCascaderKey = ref(0)
const musicCascaderProps = shallowRef({
  lazy: true,
  checkStrictly: true, // 允许选择任意层级（含根目录下的直接子目录）
  lazyLoad: async (node, resolve) => {
    if (!musicForm.serverId) { resolve([]); return }
    const dir = node.level === 0 ? '/' : node.data.value
    try {
      const items = (await listFiles(musicForm.serverId, dir)) || []
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

function onMusicServerChange() {
  musicCascaderValue.value = []
  musicForm.musicDir = '/'
  musicCascaderKey.value++ // 切换服务器强制重建级联选择器
}

function onMusicCascaderChange(val) {
  if (Array.isArray(val) && val.length > 0) {
    musicForm.musicDir = val[val.length - 1]
  } else {
    musicForm.musicDir = '/'
  }
}

// /a/b/c → ['/a', '/a/b', '/a/b/c']，用于把已保存目录回填到级联选择器
function pathToCascaderValue(p) {
  const segs = (p || '/').split('/').filter(Boolean)
  return segs.map((_, i) => '/' + segs.slice(0, i + 1).join('/'))
}

async function fetchStatus() {
  try {
    status.value = await getBotStatus()
  } catch (e) { /* ignore */ }
}

async function fetchQR() {
  loadingQR.value = true
  try {
    const res = await getBotQRCode()
    qrCode.value = res.qrcode
    qrCodeImgContent.value = res.qrcode_img_content
    qrUrl.value = res.qrcode_img_content
    // 微信返回的是 H5 页面地址（非图片），按官方页面做法把完整 URL 本地生成二维码
    qrImg.value = await QRCode.toDataURL(qrUrl.value, { width: 260, margin: 2 })
    confirming.value = true
    // 轮询确认登录
    pollConfirm()
  } catch (e) {
    ElMessage.error('获取二维码失败')
  } finally {
    loadingQR.value = false
  }
}

async function pollConfirm() {
  if (!confirming.value) return
  try {
    await confirmBotLogin({ qrcode: qrCode.value, qrcodeImgContent: qrCodeImgContent.value })
    ElMessage.success('登录成功')
    resetQR()
    fetchStatus()
  } catch (e) {
    // 401 登录态失效：拦截器已跳转登录页，停止轮询避免无限重试
    if (e?.response?.status === 401) {
      confirming.value = false
      return
    }
    // 超时或其他错误，3 秒后继续轮询
    if (confirming.value) {
      pollTimer = setTimeout(pollConfirm, 3000)
    }
  }
}

let pollTimer = null

function resetQR() {
  confirming.value = false
  if (pollTimer) { clearTimeout(pollTimer); pollTimer = null }
  qrUrl.value = ''
  qrImg.value = ''
  qrCode.value = ''
  qrCodeImgContent.value = ''
}

async function onStart() {
  await startBot()
  ElMessage.success('机器人已启动')
  fetchStatus()
}

async function onStop() {
  await stopBot()
  ElMessage.success('机器人已停止')
  fetchStatus()
}

async function onLogout() {
  resetQR()
  status.value.loggedIn = false
  status.value.status = 'offline'
}

async function loadMusicConfig() {
  try {
    const cfg = await getMusicConfig()
    musicForm.serverId = cfg.serverId || 0
    musicForm.musicDir = cfg.musicDir || '/'
    musicCascaderValue.value = musicForm.serverId ? pathToCascaderValue(musicForm.musicDir) : []
    musicCascaderKey.value++ // 按已保存服务器重建级联选择器
  } catch (e) { /* ignore */ }
}

async function onSaveMusic() {
  if (!musicForm.serverId) { ElMessage.warning('请选择目标服务器'); return }
  savingMusic.value = true
  try {
    await saveMusicConfig(musicForm)
    ElMessage.success('音乐配置已保存')
  } finally {
    savingMusic.value = false
  }
}

onMounted(async () => {
  fetchStatus()
  servers.value = (await listServers()) || []
  loadMusicConfig()
  fetchQuarkStatus()
})

// ---- 夸克网盘 ----
const quarkStatus = ref({ configured: false, nickname: '', valid: false })
const quarkLoadingQR = ref(false)
const quarkQrImg = ref('')
const quarkToken = ref('')
const quarkCookieInput = ref('')
const quarkSavingCookie = ref(false)
const quarkLoggingOut = ref(false)
let quarkPollTimer = null

async function fetchQuarkStatus() {
  try {
    quarkStatus.value = await getQuarkStatus()
  } catch (e) { /* ignore */ }
}

async function fetchQuarkQR() {
  quarkLoadingQR.value = true
  try {
    const res = await getQuarkQRCode()
    quarkToken.value = res.token
    quarkQrImg.value = await QRCode.toDataURL(res.qrUrl, { width: 260, margin: 2 })
    pollQuarkLogin()
  } catch (e) {
    ElMessage.error('获取夸克登录二维码失败')
  } finally {
    quarkLoadingQR.value = false
  }
}

async function pollQuarkLogin() {
  if (!quarkToken.value) return
  try {
    const res = await pollQuarkQRCode(quarkToken.value)
    if (res.status === 'success') {
      ElMessage.success('夸克网盘登录成功')
      resetQuarkQR()
      fetchQuarkStatus()
      return
    }
    if (res.status === 'expired') {
      ElMessage.warning('二维码已过期，请重新获取')
      resetQuarkQR()
      return
    }
    // waiting → 继续轮询
  } catch (e) {
    if (e?.response?.status === 401) return // 登录态失效，拦截器已处理
  }
  quarkPollTimer = setTimeout(pollQuarkLogin, 2000)
}

function resetQuarkQR() {
  quarkToken.value = ''
  quarkQrImg.value = ''
  if (quarkPollTimer) { clearTimeout(quarkPollTimer); quarkPollTimer = null }
}

async function onSaveQuarkCookie() {
  if (!quarkCookieInput.value.trim()) { ElMessage.warning('请粘贴 Cookie'); return }
  quarkSavingCookie.value = true
  try {
    await saveQuarkCookie(quarkCookieInput.value.trim())
    ElMessage.success('夸克 Cookie 已保存')
    quarkCookieInput.value = ''
    fetchQuarkStatus()
  } finally {
    quarkSavingCookie.value = false
  }
}

async function onQuarkLogout() {
  quarkLoggingOut.value = true
  try {
    await quarkLogout()
    ElMessage.success('已解绑夸克网盘')
    fetchQuarkStatus()
  } finally {
    quarkLoggingOut.value = false
  }
}

onUnmounted(() => {
  confirming.value = false
  if (pollTimer) clearTimeout(pollTimer)
  resetQuarkQR()
})
</script>
