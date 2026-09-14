import { create } from 'zustand'

type ThemeMode = 'dark' | 'light'
const storageKey = 'threadmill-theme'
function apply(mode: ThemeMode) {
  document.documentElement.classList.toggle('dark', mode === 'dark')
  document.documentElement.dataset.theme = mode
}
function initialMode(): ThemeMode {
  let mode: ThemeMode = 'dark'
  try { if (localStorage.getItem(storageKey) === 'light') mode = 'light' } catch { /* Storage may be disabled. */ }
  apply(mode)
  return mode
}

export const useThemeStore = create<{ mode: ThemeMode; toggle: () => void }>((set, get) => ({
  mode: initialMode(),
  toggle() {
    const mode = get().mode === 'dark' ? 'light' : 'dark'
    try { localStorage.setItem(storageKey, mode) } catch { /* The current session still changes theme. */ }
    apply(mode)
    set({ mode })
  },
}))
