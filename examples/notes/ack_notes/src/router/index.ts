// 路由定义：'/' 为空白聊天页，'/c/:threadId' 打开指定会话。

import { createRouter, createWebHistory } from 'vue-router'
import ChatView from '@/views/ChatView.vue'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: '/',
      name: 'home',
      component: ChatView,
    },
    {
      path: '/c/:threadId',
      name: 'chat',
      component: ChatView,
    },
  ],
})

export default router
