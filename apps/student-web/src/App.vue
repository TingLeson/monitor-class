<script setup lang="ts">
import { AppShell } from '@classwatch/ui'
import { RouterView } from 'vue-router'

/**
 * 学生端根组件：只负责"外壳 + 路由出口"。
 *
 * WHY 这里没有任何角色判断：学生端是一个物理独立的 SPA（§5），
 * 它的进程里不存在教师/管理员的页面代码，因此也就不可能因为前端 bug
 * 把教师界面渲染给学生。
 */
</script>

<template>
  <AppShell subtitle="学生端">
    <RouterView />
  </AppShell>
</template>
