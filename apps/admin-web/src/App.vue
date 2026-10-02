<script setup lang="ts">
import { AppShell } from '@classwatch/ui'
import { RouterView } from 'vue-router'

/**
 * 管理端根组件：只负责"外壳 + 路由出口"。
 *
 * WHY 这里没有任何角色判断：管理端是一个物理独立的 SPA（§5）。
 * 更关键的是——管理员的会话与页面永远不会被打包进学生端/教师端的产物里，
 * 学生即使改前端代码也拿不到管理接口的页面逻辑（接口本身仍由后端 RBAC 保护，§37）。
 */
</script>

<template>
  <AppShell subtitle="管理端">
    <RouterView />
  </AppShell>
</template>
