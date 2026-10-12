<template>
  <el-card>
    <template #header>
      <div style="display:flex;align-items:center;justify-content:space-between">
        <span>微信 / QQ 机器人</span>
        <el-button type="primary" size="small" @click="openAddDialog">添加机器人</el-button>
      </div>
    </template>

    <!-- 机器人账号列表 -->
    <el-empty v-if="accounts.length === 0" description="还没有绑定机器人，点击右上角「添加机器人」绑定微信或 QQ" :image-size="80" />
    <el-table v-else :data="accounts" style="width:100%">
      <el-table-column label="渠道" width="90">
        <template #default="{ row }">
          <el-tag :type="row.channel === 'qq' ? 'primary' : 'success'">{{ row.channel === 'qq' ? 'QQ' : '微信' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="备注名" min-width="140">
        <template #default="{ row }">
          <span style="cursor:pointer" @click="onRename(row)">{{ row.name || '未命名' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="标识" min-width="180">
        <template #default="{ row }">
          <span v-if="row.channel === 'qq'">{{ row.appId }}<el-tag v-if="row.sandbox" size="small" type="warning" style="margin-left:4px">沙箱</el-tag></span>
          <span v-else>{{ row.botId || '-' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="110">
        <template #default="{ row }">
          <el-tag :type="statusTagType(row)">{{ statusText(row) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="280">
        <template #default="{ row }">
          <template v-if="row.channel === 'qq'">
            <el-button v-if="!row.loggedIn" size="small" type="primary" @click="openEditCreds(row)">配置凭据</el-button>
            <el-button v-else-if="row.running" size="small" type="danger" @click="onStop(row)">停止</el-button>
            <el-button v-else size="small" type="primary" @click="onStart(row)">启动</el-button>
            <el-button v-if="row.loggedIn" size="small" @click="openEditCreds(row)">编辑凭据</el-button>
          </template>
          <template v-else>
            <el-button v-if="!row.loggedIn" size="small" type="primary" @click="onRelogin(row)">扫码登录</el-button>
            <el-button v-else-if="row.running" size="small" type="danger" @click="onStop(row)">停止</el-button>
            <el-button v-else size="small" type="primary" @click="onStart(row)">启动</el-button>
            <el-button v-if="row.loggedIn" size="small" @click="onRelogin(row)">重新登录</el-button>
          </template>
          <el-button size="small" type="danger" plain @click="onDelete(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 添加机器人弹窗（选择渠道） -->
    <el-dialog v-model="addDialogVisible" title="添加机器人" width="440px" :close-on-click-modal="false">
      <el-form label-width="90px">
        <el-form-item label="渠道">
          <el-radio-group v-model="addForm.channel">
            <el-radio-button label="wechat">微信（扫码）</el-radio-button>
            <el-radio-button label="qq">QQ（凭据）</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="备注名">
          <el-input v-model="addForm.name" placeholder="备注名（可留空）" maxlength="30" />
        </el-form-item>
        <template v-if="addForm.channel === 'qq'">
          <el-form-item label="AppID">
            <el-input v-model="addForm.appId" placeholder="QQ 开放平台 AppID" />
          </el-form-item>
          <el-form-item label="AppSecret">
            <el-input v-model="addForm.clientSecret" placeholder="QQ 开放平台 AppSecret" show-password />
          </el-form-item>
          <el-form-item label="沙箱">
            <el-switch v-model="addForm.sandbox" />
            <span style="margin-left:8px;color:#999;font-size:12px">机器人未上线时勾选（沙箱环境）</span>
          </el-form-item>
          <el-alert type="info" :closable="false">
            前往 <el-link href="https://q.qq.com/" target="_blank" type="primary" :underline="false">QQ 开放平台</el-link>
            创建机器人并获取 AppID / AppSecret；需要在机器人管理后台配置服务器 IP（白名单）。
          </el-alert>
        </template>
      </el-form>
      <template #footer>
        <el-button @click="addDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="adding" @click="submitAdd">
          {{ addForm.channel === 'qq' ? '保存并启动' : '下一步（扫码）' }}
        </el-button>
      </template>
    </el-dialog>

    <!-- QQ 凭据编辑弹窗 -->
    <el-dialog v-model="credDialogVisible" title="编辑 QQ 机器人凭据" width="440px" :close-on-click-modal="false">
      <el-form label-width="90px">
        <el-form-item label="备注名">
          <el-input v-model="credForm.name" placeholder="备注名" maxlength="30" />
        </el-form-item>
        <el-form-item label="AppID">
          <el-input v-model="credForm.appId" placeholder="QQ 开放平台 AppID" />
        </el-form-item>
        <el-form-item label="AppSecret">
          <el-input v-model="credForm.clientSecret" placeholder="留空沿用旧值" show-password />
        </el-form-item>
        <el-form-item label="沙箱">
          <el-switch v-model="credForm.sandbox" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="credDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="savingCreds" @click="submitCreds">保存</el-button>
      </template>
    </el-dialog>

    <!-- 扫码登录弹窗 -->
    <el-dialog v-model="qrDialogVisible" :title="qrTitle" width="360px" :close-on-click-modal="false" @closed="resetQR">
      <div v-if="qrImg" style="text-align:center;padding:10px">
        <img :src="qrImg" alt="登录二维码" style="width:260px;height:260px;border:1px solid #eee;border-radius:8px" />
        <div style="margin-top:12px;color:#666">请使用微信扫描二维码</div>
        <div v-if="confirming" style="margin-top:8px;color:#e6a23c">等待确认登录...</div>
        <div style="margin-top:12px">
          <el-button size="small" @click="fetchQR">刷新二维码</el-button>
          <el-link v-if="qrUrl" :href="qrUrl" target="_blank" type="primary" :underline="false" style="margin-left:8px">二维码异常？在新窗口打开</el-link>
        </div>
      </div>
      <div v-else style="text-align:center;padding:30px">
        <el-button type="primary" :loading="loadingQR" @click="fetchQR">获取登录二维码</el-button>
      </div>
    </el-dialog>

    <!-- 音乐配置 -->
    <el-card shadow="never" style="margin-top:16px">
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
        <p>音乐目录由所有绑定的机器人（微信 / QQ）共用；支持逐级选择，清空选择则使用服务器根目录「/」，机器人会在该目录下递归查找音频。</p>
        <p>在微信或 QQ 中向机器人发送以下指令：</p>
        <p><b>听音乐</b> — 随机播放一首音乐</p>
        <p><b>听音乐 歌名</b> — 搜索并播放指定歌曲；本地没有时自动在线搜索，回复序号即可下载播放（需绑定夸克网盘）</p>
        <p>QQ 机器人：私聊直接发指令；群聊中需 <b>@机器人</b> 后跟指令。QQ 群聊/私聊的音频以文件消息发送（QQ 官方限制语音需 silk 格式）。</p>
      </el-alert>
    </el-card>

    <!-- 电视剧目录配置（看电视） -->
    <el-card shadow="never" style="margin-top:16px">
      <template #header><span>电视剧目录配置（看电视）</span></template>
      <el-form :model="tvForm" label-width="100px" style="max-width:600px">
        <el-form-item label="目标服务器">
          <el-select v-model="tvForm.serverId" placeholder="选择NAS服务器" style="width:100%" @change="onTvServerChange">
            <el-option v-for="s in servers" :key="s.id" :label="`${s.name} (${s.protocol.toUpperCase()} ${s.host})`" :value="s.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="电视剧目录">
          <el-cascader
            v-model="tvCascaderValue"
            :props="tvCascaderProps"
            :key="tvCascaderKey"
            :disabled="!tvForm.serverId"
            placeholder="逐级选择电视剧目录"
            clearable
            filterable
            style="width:100%"
            @change="onTvCascaderChange"
          />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="savingTv" @click="onSaveTv">保存配置</el-button>
        </el-form-item>
      </el-form>
      <el-alert type="info" :closable="false" style="margin-top:12px">
        <p>电视剧目录由所有绑定的机器人共用；按<b>子文件夹=剧名</b>、文件夹内视频文件=剧集组织。</p>
        <p>在微信或 QQ 中向机器人发送以下指令（<b>仅使用 NAS 本地目录，不走网络搜索</b>）：</p>
        <p><b>看电视</b> — 列出所有电视剧文件夹，回复序号选剧再选集</p>
        <p><b>看电视 剧名</b> — 按剧名搜索，选剧后选集</p>
      </el-alert>
    </el-card>

    <!-- 电影目录配置（看电影） -->
    <el-card shadow="never" style="margin-top:16px">
      <template #header><span>电影目录配置（看电影）</span></template>
      <el-form :model="movieForm" label-width="100px" style="max-width:600px">
        <el-form-item label="目标服务器">
          <el-select v-model="movieForm.serverId" placeholder="选择NAS服务器" style="width:100%" @change="onMovieServerChange">
            <el-option v-for="s in servers" :key="s.id" :label="`${s.name} (${s.protocol.toUpperCase()} ${s.host})`" :value="s.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="电影目录">
          <el-cascader
            v-model="movieCascaderValue"
            :props="movieCascaderProps"
            :key="movieCascaderKey"
            :disabled="!movieForm.serverId"
            placeholder="逐级选择电影目录"
            clearable
            filterable
            style="width:100%"
            @change="onMovieCascaderChange"
          />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="savingMovie" @click="onSaveMovie">保存配置</el-button>
        </el-form-item>
      </el-form>
      <el-alert type="info" :closable="false" style="margin-top:12px">
        <p>电影目录由所有绑定的机器人共用；该目录下的所有视频文件（递归）视为电影。</p>
        <p>在微信或 QQ 中向机器人发送以下指令（<b>仅使用 NAS 本地目录，不走网络搜索</b>）：</p>
        <p><b>看电影 片名</b> — 按片名搜索，回复序号选择</p>
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
import { ElMessage, ElMessageBox } from 'element-plus'
import QRCode from 'qrcode'
import {
  getBotAccounts, createBotAccount, updateBotAccount, deleteBotAccount,
  getBotQRCode, confirmBotLogin, startBot, stopBot,
  getMusicConfig, saveMusicConfig,
  getVideoConfig, saveVideoConfig,
  listServers, listFiles,
  getQuarkStatus, saveQuarkCookie, quarkLogout, getQuarkQRCode, pollQuarkQRCode
} from '../api'

// ---- 机器人账号列表 ----
const accounts = ref([])

function statusText(row) {
  if (row.running) return '在线'
  if (!row.loggedIn) return row.channel === 'qq' ? '未配置' : '未登录'
  if (row.status === 'expired') return '已过期'
  return '已停止'
}

function statusTagType(row) {
  if (row.running) return 'success'
  if (!row.loggedIn) return 'info'
  if (row.status === 'expired') return 'danger'
  return 'warning'
}

async function fetchAccounts() {
  try {
    accounts.value = (await getBotAccounts()) || []
  } catch (e) { /* ignore */ }
}

// ---- 添加机器人弹窗（渠道选择） ----
const addDialogVisible = ref(false)
const adding = ref(false)
const addForm = reactive({ channel: 'wechat', name: '', appId: '', clientSecret: '', sandbox: false })

function openAddDialog() {
  addForm.channel = 'wechat'
  addForm.name = ''
  addForm.appId = ''
  addForm.clientSecret = ''
  addForm.sandbox = false
  addDialogVisible.value = true
}

async function submitAdd() {
  if (addForm.channel === 'qq') {
    if (!addForm.appId.trim() || !addForm.clientSecret.trim()) {
      ElMessage.warning('请填写 AppID 与 AppSecret')
      return
    }
    adding.value = true
    try {
      const res = await createBotAccount({
        channel: 'qq',
        name: addForm.name.trim(),
        appId: addForm.appId.trim(),
        clientSecret: addForm.clientSecret.trim(),
        sandbox: addForm.sandbox,
      })
      addDialogVisible.value = false
      fetchAccounts()
      // 创建成功后自动启动（凭据错误会在启动接口返回）
      try {
        await startBot(res.id)
        ElMessage.success('QQ 机器人已添加并启动')
      } catch (e2) {
        ElMessage.warning(e2?.response?.data?.message || '已添加，但启动失败，请检查凭据或 IP 白名单')
      }
      fetchAccounts()
    } catch (e) {
      ElMessage.error(e?.response?.data?.message || '添加 QQ 机器人失败')
    } finally {
      adding.value = false
    }
    return
  }
  // 微信：创建账号后进入扫码流程
  adding.value = true
  try {
    const res = await createBotAccount({ name: addForm.name.trim() })
    addDialogVisible.value = false
    qrAccountID.value = res.id
    qrTitle.value = addForm.name.trim() ? `扫码绑定「${addForm.name.trim()}」` : '扫码绑定微信'
    qrDialogVisible.value = true
    fetchQR()
  } catch (e) {
    ElMessage.error('创建微信账号失败')
  } finally {
    adding.value = false
  }
}

// 已有账号重新扫码登录
function onRelogin(row) {
  qrAccountID.value = row.id
  qrTitle.value = `重新登录「${row.name || '微信'}」`
  qrDialogVisible.value = true
  resetQR()
}

// 重命名
async function onRename(row) {
  try {
    const res = await ElMessageBox.prompt('修改备注名', '备注名', {
      confirmButtonText: '保存',
      cancelButtonText: '取消',
      inputValue: row.name || '',
    })
    const name = (res.value || '').trim()
    if (!name) { ElMessage.warning('备注名不能为空'); return }
    await updateBotAccount(row.id, { name })
    fetchAccounts()
  } catch (e) { /* 取消 */ }
}

// ---- QQ 凭据编辑 ----
const credDialogVisible = ref(false)
const savingCreds = ref(false)
const credForm = reactive({ id: 0, name: '', appId: '', clientSecret: '', sandbox: false, running: false })

function openEditCreds(row) {
  credForm.id = row.id
  credForm.name = row.name || ''
  credForm.appId = row.appId || ''
  credForm.clientSecret = '' // 留空沿用旧值
  credForm.sandbox = !!row.sandbox
  credForm.running = !!row.running
  credDialogVisible.value = true
}

async function submitCreds() {
  if (!credForm.appId.trim()) { ElMessage.warning('AppID 不能为空'); return }
  savingCreds.value = true
  try {
    await updateBotAccount(credForm.id, {
      name: credForm.name.trim(),
      appId: credForm.appId.trim(),
      clientSecret: credForm.clientSecret.trim(), // 空字符串后端视为沿用旧值
      sandbox: credForm.sandbox,
    })
    credDialogVisible.value = false
    ElMessage.success('凭据已保存')
    // 运行中的实例需重启才能应用新凭据
    if (credForm.running) {
      try {
        await stopBot(credForm.id)
        await startBot(credForm.id)
        ElMessage.success('机器人已用新凭据重启')
      } catch (e) {
        ElMessage.warning('凭据已保存，但自动重启失败，请手动启动')
      }
    }
    fetchAccounts()
  } catch (e) {
    ElMessage.error(e?.response?.data?.message || '保存失败')
  } finally {
    savingCreds.value = false
  }
}

// 删除账号
async function onDelete(row) {
  try {
    await ElMessageBox.confirm(
      `确定删除「${row.name || '未命名'}」吗？删除后该机器人将停止接收指令。`,
      '删除机器人', { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' })
  } catch (e) {
    return
  }
  try {
    await deleteBotAccount(row.id)
    ElMessage.success('已删除')
    fetchAccounts()
  } catch (e) {
    ElMessage.error('删除失败')
  }
}

async function onStart(row) {
  try {
    await startBot(row.id)
    ElMessage.success('机器人已启动')
    fetchAccounts()
  } catch (e) {
    ElMessage.error(e?.response?.data?.message || '启动失败')
  }
}

async function onStop(row) {
  try {
    await stopBot(row.id)
    ElMessage.success('机器人已停止')
    fetchAccounts()
  } catch (e) { /* ignore */ }
}

// ---- 扫码登录弹窗 ----
const qrDialogVisible = ref(false)
const qrTitle = ref('扫码绑定微信')
const qrAccountID = ref(0) // 当前扫码登录的账号 ID
const loadingQR = ref(false)
const qrUrl = ref('')        // 微信登录页 URL（二维码编码内容）
const qrImg = ref('')        // 本地生成的二维码图片 data URL
const qrCode = ref('')
const qrCodeImgContent = ref('')
const confirming = ref(false)
let pollTimer = null

async function fetchQR() {
  if (!qrAccountID.value) return
  loadingQR.value = true
  if (pollTimer) { clearTimeout(pollTimer); pollTimer = null }
  try {
    const res = await getBotQRCode(qrAccountID.value)
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
  if (!confirming.value || !qrAccountID.value) return
  try {
    await confirmBotLogin(qrAccountID.value, { qrcode: qrCode.value, qrcodeImgContent: qrCodeImgContent.value })
    ElMessage.success('登录成功，机器人已自动启动')
    resetQR()
    qrDialogVisible.value = false
    fetchAccounts()
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

function resetQR() {
  confirming.value = false
  if (pollTimer) { clearTimeout(pollTimer); pollTimer = null }
  qrUrl.value = ''
  qrImg.value = ''
  qrCode.value = ''
  qrCodeImgContent.value = ''
}

// ---- 音乐配置（所有微信共用） ----
const servers = ref([]) // 可选的 NAS 服务器列表
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

// ---- 影视配置（看电视/看电影，所有机器人共用，两个目录独立配置） ----
const tvForm = reactive({ serverId: 0, dir: '/' })
const savingTv = ref(false)
const tvCascaderValue = ref([])
const tvCascaderKey = ref(0)
const tvCascaderProps = shallowRef({
  lazy: true,
  checkStrictly: true,
  lazyLoad: async (node, resolve) => {
    if (!tvForm.serverId) { resolve([]); return }
    const dir = node.level === 0 ? '/' : node.data.value
    try {
      const items = (await listFiles(tvForm.serverId, dir)) || []
      resolve(items.filter(i => i.isDir).map(i => ({
        label: i.name,
        value: dir === '/' ? '/' + i.name : dir + '/' + i.name,
      })))
    } catch (e) {
      resolve([])
    }
  }
})

function onTvServerChange() {
  tvCascaderValue.value = []
  tvForm.dir = '/'
  tvCascaderKey.value++
}

function onTvCascaderChange(val) {
  tvForm.dir = (Array.isArray(val) && val.length > 0) ? val[val.length - 1] : '/'
}

const movieForm = reactive({ serverId: 0, dir: '/' })
const savingMovie = ref(false)
const movieCascaderValue = ref([])
const movieCascaderKey = ref(0)
const movieCascaderProps = shallowRef({
  lazy: true,
  checkStrictly: true,
  lazyLoad: async (node, resolve) => {
    if (!movieForm.serverId) { resolve([]); return }
    const dir = node.level === 0 ? '/' : node.data.value
    try {
      const items = (await listFiles(movieForm.serverId, dir)) || []
      resolve(items.filter(i => i.isDir).map(i => ({
        label: i.name,
        value: dir === '/' ? '/' + i.name : dir + '/' + i.name,
      })))
    } catch (e) {
      resolve([])
    }
  }
})

function onMovieServerChange() {
  movieCascaderValue.value = []
  movieForm.dir = '/'
  movieCascaderKey.value++
}

function onMovieCascaderChange(val) {
  movieForm.dir = (Array.isArray(val) && val.length > 0) ? val[val.length - 1] : '/'
}

async function loadVideoConfig() {
  try {
    const cfg = await getVideoConfig()
    tvForm.serverId = cfg.tvServerId || 0
    tvForm.dir = cfg.tvDir || '/'
    tvCascaderValue.value = tvForm.serverId ? pathToCascaderValue(tvForm.dir) : []
    tvCascaderKey.value++
    movieForm.serverId = cfg.movieServerId || 0
    movieForm.dir = cfg.movieDir || '/'
    movieCascaderValue.value = movieForm.serverId ? pathToCascaderValue(movieForm.dir) : []
    movieCascaderKey.value++
  } catch (e) { /* ignore */ }
}

async function onSaveTv() {
  if (!tvForm.serverId) { ElMessage.warning('请选择目标服务器'); return }
  savingTv.value = true
  try {
    await saveVideoConfig({
      tvServerId: tvForm.serverId,
      tvDir: tvForm.dir,
      movieServerId: movieForm.serverId,
      movieDir: movieForm.dir,
    })
    ElMessage.success('电视剧目录配置已保存')
  } finally {
    savingTv.value = false
  }
}

async function onSaveMovie() {
  if (!movieForm.serverId) { ElMessage.warning('请选择目标服务器'); return }
  savingMovie.value = true
  try {
    await saveVideoConfig({
      tvServerId: tvForm.serverId,
      tvDir: tvForm.dir,
      movieServerId: movieForm.serverId,
      movieDir: movieForm.dir,
    })
    ElMessage.success('电影目录配置已保存')
  } finally {
    savingMovie.value = false
  }
}

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

onMounted(async () => {
  fetchAccounts()
  servers.value = (await listServers()) || []
  loadMusicConfig()
  loadVideoConfig()
  fetchQuarkStatus()
})

onUnmounted(() => {
  confirming.value = false
  if (pollTimer) clearTimeout(pollTimer)
  resetQuarkQR()
})
</script>
