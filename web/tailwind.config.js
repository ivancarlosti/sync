// Tailwind configuration for the Sync SPA.
//
// Every colour is a semantic CSS variable (see `src/style.css`), which is what
// makes the light/dark switch a single class on <html> and lets the seven
// locale scripts pick their own font fallback. Logical spacing utilities
// (`ps-*`, `me-*`, `start-*`, `text-start`) plus the `rtl:`/`ltr:` variants added
// below keep the layout correct when `dir="rtl"` (ar-SA).
import animate from 'tailwindcss-animate';
import plugin from 'tailwindcss/plugin';

/** @type {import('tailwindcss').Config} */
export default {
  darkMode: ['class'],
  content: ['./index.html', './src/**/*.{vue,ts}'],
  // The shell is intentionally full-width (`w-full px-5` in AppLayout/AppHeader)
  // so the wide Jobs and Runs tables get every pixel the viewport offers instead
  // of being squeezed into a centred, capped box. The core `container` plugin is
  // disabled outright so no dead `.container` rule is ever emitted.
  corePlugins: { container: false },
  theme: {
    extend: {
      colors: {
        border: 'hsl(var(--border))',
        input: 'hsl(var(--input))',
        ring: 'hsl(var(--ring))',
        background: 'hsl(var(--background))',
        foreground: 'hsl(var(--foreground))',
        primary: {
          DEFAULT: 'hsl(var(--primary))',
          foreground: 'hsl(var(--primary-foreground))',
        },
        secondary: {
          DEFAULT: 'hsl(var(--secondary))',
          foreground: 'hsl(var(--secondary-foreground))',
        },
        destructive: {
          DEFAULT: 'hsl(var(--destructive))',
          foreground: 'hsl(var(--destructive-foreground))',
        },
        success: {
          DEFAULT: 'hsl(var(--success))',
          foreground: 'hsl(var(--success-foreground))',
        },
        warning: {
          DEFAULT: 'hsl(var(--warning))',
          foreground: 'hsl(var(--warning-foreground))',
        },
        muted: {
          DEFAULT: 'hsl(var(--muted))',
          foreground: 'hsl(var(--muted-foreground))',
        },
        accent: {
          DEFAULT: 'hsl(var(--accent))',
          foreground: 'hsl(var(--accent-foreground))',
        },
        popover: {
          DEFAULT: 'hsl(var(--popover))',
          foreground: 'hsl(var(--popover-foreground))',
        },
        card: {
          DEFAULT: 'hsl(var(--card))',
          foreground: 'hsl(var(--card-foreground))',
        },
      },
      borderRadius: {
        xl: 'calc(var(--radius) + 4px)',
        lg: 'var(--radius)',
        md: 'calc(var(--radius) - 2px)',
        sm: 'calc(var(--radius) - 4px)',
      },
      fontFamily: {
        // No webfont is fetched: the app must work offline and behind a strict
        // CSP. The stack lists one face per script of the shipped locales.
        sans: [
          'system-ui',
          '-apple-system',
          'Segoe UI',
          'Roboto',
          'Noto Sans',
          'Noto Sans SC',
          'Noto Sans Devanagari',
          'Noto Sans Arabic',
          'Helvetica Neue',
          'Arial',
          'sans-serif',
        ],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
      keyframes: {
        'accordion-down': {
          from: { height: '0' },
          to: { height: 'var(--reka-accordion-content-height)' },
        },
        'accordion-up': {
          from: { height: 'var(--reka-accordion-content-height)' },
          to: { height: '0' },
        },
      },
      animation: {
        'accordion-down': 'accordion-down 0.2s ease-out',
        'accordion-up': 'accordion-up 0.2s ease-out',
      },
    },
  },
  plugins: [
    animate,
    plugin(({ addVariant }) => {
      // Tailwind 3 has no direction variants, and the header, tables and forms
      // need to flip padding, borders and text alignment for ar-SA.
      addVariant('rtl', '&:where([dir="rtl"], [dir="rtl"] *)');
      addVariant('ltr', '&:where([dir="ltr"], [dir="ltr"] *)');
    }),
  ],
};
