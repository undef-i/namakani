import { createApp } from 'vue'
import { createVuetify } from 'vuetify'
import { en, zhHans, zhHant } from 'vuetify/locale'
import 'vuetify/styles'
import './styles.css'
import '@mdi/font/css/materialdesignicons.css'
import '@fontsource/roboto/400.css'
import '@fontsource/roboto/500.css'
import '@fontsource/roboto/700.css'
import App from './App.vue'
import { initScrollbars } from './scrollbars.js'
import { lang } from './i18n'

const vuetify = createVuetify({
  defaults: {
    VTextField: { variant: 'filled' },
    VTextarea: { variant: 'filled' },
    VSelect: { variant: 'filled' },
    VAutocomplete: { variant: 'filled' },
    VCombobox: { variant: 'filled' },
  },
  locale: {
    locale: lang.value === 'zh-Hant' ? 'zhHant' : lang.value === 'zh-Hans' ? 'zhHans' : 'en',
    fallback: 'en',
    messages: { zhHans, zhHant, en },
  },
  theme: {
    defaultTheme: 'light',
    themes: {
      light: {
        dark: false,
        colors: {
          primary: '#0B57D0',
          'on-primary': '#FFFFFF',
          'primary-container': '#D3E3FD',
          'on-primary-container': '#041E49',
          secondary: '#5F6368',
          'secondary-container': '#DAE4F5',
          'on-secondary-container': '#131B2C',
          surface: '#F8FAFD',
          'on-surface': '#1B1C1E',
          'surface-container-low': '#EFF3F8',
          'surface-container': '#E7EDF4',
          'surface-container-high': '#DEE6F0',
          'surface-variant': '#E1E5EA',
          'on-surface-variant': '#43474C',
          outline: '#74787C',
          'outline-variant': '#C3C8CD',
          background: '#E7EDF4',
          'on-background': '#1B1C1E',
          error: '#B3261E',
          'on-error': '#FFFFFF',
          'error-container': '#F9DEDC',
          'on-error-container': '#410E0B',
          success: '#188038',
          warning: '#E37400',
          info: '#0B57D0',
        },
      },
      dark: {
        dark: true,
        colors: {
          primary: '#A8C7FA',
          'on-primary': '#0A2E5C',
          'primary-container': '#12497E',
          'on-primary-container': '#D3E3FD',
          secondary: '#9AA0A6',
          'secondary-container': '#3E4759',
          'on-secondary-container': '#DAE4F5',
          surface: '#131415',
          'on-surface': '#E3E3E6',
          'surface-container-low': '#1A1B1D',
          'surface-container': '#1F2022',
          'surface-container-high': '#2A2B2D',
          'surface-variant': '#2A2B2D',
          'on-surface-variant': '#C4C7CA',
          outline: '#8E9196',
          'outline-variant': '#44474A',
          background: '#1F2022',
          'on-background': '#E3E3E6',
          error: '#F2B8B5',
          'on-error': '#601410',
          'error-container': '#93000A',
          'on-error-container': '#F9DEDC',
          success: '#81C995',
          warning: '#FDD663',
          info: '#A8C7FA',
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
initScrollbars()
