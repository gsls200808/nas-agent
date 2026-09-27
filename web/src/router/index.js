import { createRouter, createWebHistory } from 'vue-router'

const routes = [
  { path: '/login', component: () => import('../views/Login.vue') },
  {
    path: '/',
    component: () => import('../views/Layout.vue'),
    redirect: '/servers',
    children: [
      { path: 'servers', component: () => import('../views/Servers.vue'), meta: { title: '服务器管理' } },
      { path: 'files', component: () => import('../views/Files.vue'), meta: { title: '文件浏览' } },
      { path: 'transfers', component: () => import('../views/Transfers.vue'), meta: { title: '转存任务' } },
      { path: 'bot', component: () => import('../views/Bot.vue'), meta: { title: '微信机器人' } },
      { path: 'users', component: () => import('../views/Users.vue'), meta: { title: '用户管理' } }
    ]
  }
]

const router = createRouter({ history: createWebHistory(), routes })

router.beforeEach((to) => {
  if (to.path !== '/login' && !localStorage.getItem('token')) return '/login'
})

export default router
