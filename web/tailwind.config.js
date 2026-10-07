/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'media',
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      fontFamily: {
        sans: [
          '"Noto Sans SC"',
          'system-ui',
          '-apple-system',
          '"Segoe UI"',
          'Roboto',
          '"PingFang SC"',
          '"Microsoft YaHei"',
          'sans-serif',
        ],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
      colors: {
        // 中性 slate（继承自 Tailwind 默认调色板，此处仅做语义别名）
        surface: {
          DEFAULT: 'rgb(var(--ez-surface) / <alpha-value>)',
          raised: 'rgb(var(--ez-surface-raised) / <alpha-value>)',
          sunken: 'rgb(var(--ez-surface-sunken) / <alpha-value>)',
        },
        line: 'rgb(var(--ez-line) / <alpha-value>)',
        ink: {
          DEFAULT: 'rgb(var(--ez-ink) / <alpha-value>)',
          muted: 'rgb(var(--ez-ink-muted) / <alpha-value>)',
          faint: 'rgb(var(--ez-ink-faint) / <alpha-value>)',
        },
        brand: {
          DEFAULT: 'rgb(var(--ez-brand) / <alpha-value>)',
          soft: 'rgb(var(--ez-brand-soft) / <alpha-value>)',
        },
      },
      boxShadow: {
        card: '0 1px 2px 0 rgb(15 23 42 / 0.04), 0 1px 3px 0 rgb(15 23 42 / 0.06)',
        'card-hover': '0 4px 12px -2px rgb(15 23 42 / 0.10), 0 2px 6px -2px rgb(15 23 42 / 0.06)',
        pop: '0 12px 32px -8px rgb(15 23 42 / 0.18)',
      },
      keyframes: {
        'fade-in': {
          from: { opacity: '0' },
          to: { opacity: '1' },
        },
        'slide-up': {
          from: { opacity: '0', transform: 'translateY(6px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
        shimmer: {
          '100%': { transform: 'translateX(100%)' },
        },
      },
      animation: {
        'fade-in': 'fade-in 160ms ease-out both',
        'slide-up': 'slide-up 180ms cubic-bezier(0.16, 1, 0.3, 1) both',
      },
    },
  },
  plugins: [],
};