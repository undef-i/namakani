<script setup>
import { ref, computed } from 'vue'
import { t } from '../i18n'
import { describeRawError } from '../api.js'

const props = defineProps({ text: { type: String, default: '' }, full: { type: Boolean, default: false } })
const open = ref(false)
const summary = computed(() => describeRawError(props.text))
const isUserCopy = computed(() => !summary.value && /[\u4e00-\u9fff]/.test(props.text))
</script>

<template>
  <span v-if="isUserCopy">{{ text }}</span>
  <span v-else-if="full">
    <span v-if="summary">{{ t(summary) }}</span>
    <span v-else>{{ t('执行失败') }}</span>
    <span class="d-block text-caption mt-1 raw-error-text">{{ text }}</span>
  </span>
  <span v-else>
    <span v-if="summary">{{ t(summary) }}</span>
    <span v-else>{{ t('执行失败') }}</span>
    <v-btn variant="text" size="x-small" class="ml-1" @click="open = !open">{{ t('详情') }}</v-btn>
    <span v-if="open" class="d-block text-caption mt-1 raw-error-text">{{ text }}</span>
  </span>
</template>

<style scoped>
.raw-error-text {
  white-space: pre-wrap;
  word-break: break-all;
  font-family: monospace;
}
</style>
