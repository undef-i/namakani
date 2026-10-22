import { createApp } from 'vue'
import { createVuetify } from 'vuetify'
import { zhHans } from 'vuetify/locale'
import 'vuetify/styles'
import '@mdi/font/css/materialdesignicons.css'
import '@fontsource/roboto/400.css'
import '@fontsource/roboto/500.css'
import '@fontsource/roboto/700.css'
import App from './App.vue'

const vuetify = createVuetify({
  locale: {
    locale: 'zhHans',
    fallback: 'zhHans',
    messages: { zhHans },
  },
  theme: {
    defaultTheme: 'light',
    themes: {
      light: {
        colors: {
          primary: '#0b57d0',
          secondary: '#5f6368',
          surface: '#ffffff',
          background: '#f8f9fa',
        },
      },
      dark: {
        colors: {
          primary: '#a8c7fa',
          secondary: '#9aa0a6',
        },
      },
    },
  },
})

if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {})
  })
}

createApp(App).use(vuetify).mount('#app')
