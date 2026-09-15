<script setup lang="ts">
import SessionSidebar from '@/components/SessionSidebar.vue'
</script>

<template>
  <div class="app-shell">
    <SessionSidebar />
    <RouterView />
  </div>
</template>

<style scoped>
.app-shell {
  height: 100vh;
  display: flex;
}
</style>
