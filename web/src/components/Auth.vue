<script setup>
import { ref } from 'vue'
import { t } from '../i18n'
import * as api from '../api.js'

const props = defineProps({ mode: { type: String, default: 'login' } })
const emit = defineEmits(['authed'])

const password = ref('')
const confirm = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  if (props.mode === 'setup' && password.value !== confirm.value) {
    error.value = t('两次输入的密码不一致')
    return
  }
  busy.value = true
  try {
    if (props.mode === 'setup') await api.setup(password.value)
    else await api.login(password.value)
    emit('authed')
  } catch (err) {
    error.value = t(err.message)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <v-container class="fill-height d-flex align-center justify-center">
    <v-card width="100%" max-width="380" class="pa-2">
      <v-card-title class="text-h5">namakani</v-card-title>
      <v-card-subtitle v-if="mode === 'setup'">{{ t('首次使用：设置主密码') }}</v-card-subtitle>
      <v-card-text>
        <v-form @submit.prevent="submit">
          <v-text-field
            v-model="password"
            :label="t('密码')"
            type="password"
            variant="filled"
            :autocomplete="mode === 'setup' ? 'new-password' : 'current-password'"
            required
            class="mb-2"
          />
          <v-text-field
            v-if="mode === 'setup'"
            v-model="confirm"
            :label="t('再次输入')"
            type="password"
            variant="filled"
            autocomplete="new-password"
            required
            class="mb-2"
          />
          <v-alert v-if="error" type="error" variant="tonal" density="compact" class="mb-3">
            {{ error }}
          </v-alert>
          <v-btn type="submit" color="primary" block :loading="busy">
            {{ mode === 'setup' ? t('设置并进入') : t('登录') }}
          </v-btn>
          <p v-if="mode === 'setup'" class="text-caption text-medium-emphasis mt-3">
            {{ t('主密码用于解锁所有邮箱账号的密码。登录状态会持久保存，只有手动退出才需要重新输入。遗忘主密码无法恢复，只能重置数据。') }}
          </p>
        </v-form>
      </v-card-text>
    </v-card>
  </v-container>
</template>
