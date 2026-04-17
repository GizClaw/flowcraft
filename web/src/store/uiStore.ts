import { create } from 'zustand';
import i18n from '../i18n';

type ThemeMode = 'light' | 'dark' | 'system';
export type Language = 'en' | 'zh';

interface UIState {
  themeMode: ThemeMode;
  language: Language;
  sidebarOpen: boolean;
  copilotOpen: boolean;
  setThemeMode: (mode: ThemeMode) => void;
  setLanguage: (lang: Language) => void;
  toggleSidebar: () => void;
  toggleCopilot: () => void;
  setCopilotOpen: (open: boolean) => void;
  isDark: () => boolean;
}

function getStoredThemeMode(): ThemeMode {
  const stored = localStorage.getItem('flowcraft_theme_mode');
  if (stored === 'light' || stored === 'dark' || stored === 'system') return stored;
  return 'system';
}

function resolveIsDark(mode: ThemeMode): boolean {
  if (mode === 'system') return window.matchMedia('(prefers-color-scheme: dark)').matches;
  return mode === 'dark';
}

function applyTheme(mode: ThemeMode) {
  document.documentElement.classList.toggle('dark', resolveIsDark(mode));
}

function getStoredLanguage(): Language {
  const stored = localStorage.getItem('flowcraft_language');
  if (stored === 'en' || stored === 'zh') return stored;
  const browserLang = navigator.language.toLowerCase();
  return browserLang.startsWith('zh') ? 'zh' : 'en';
}

export const useUIStore = create<UIState>((set, get) => ({
  themeMode: getStoredThemeMode(),
  language: getStoredLanguage(),
  sidebarOpen: true,
  copilotOpen: false,

  setThemeMode: (mode) => {
    localStorage.setItem('flowcraft_theme_mode', mode);
    applyTheme(mode);
    set({ themeMode: mode });
  },

  setLanguage: (lang) => {
    localStorage.setItem('flowcraft_language', lang);
    i18n.changeLanguage(lang);
    set({ language: lang });
  },

  toggleSidebar: () => set({ sidebarOpen: !get().sidebarOpen }),
  toggleCopilot: () => set({ copilotOpen: !get().copilotOpen }),
  setCopilotOpen: (open) => set({ copilotOpen: open }),
  isDark: () => resolveIsDark(get().themeMode),
}));

// Apply theme on load
applyTheme(getStoredThemeMode());

// Listen for system theme changes when in 'system' mode
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (useUIStore.getState().themeMode === 'system') {
    applyTheme('system');
    useUIStore.setState({});
  }
});
