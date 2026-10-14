/** @type {import('tailwindcss').Config} */
export default {
    content: ["./index.html", "./src/**/*.{svelte,js,ts,jsx,tsx}"],
    theme: {
        extend: {
            colors: {
                primary: 'var(--color-primary)',
                surface: 'var(--color-surface)',
                elevated: 'var(--color-elevated)',
                border: 'var(--color-border)',
                background: 'var(--color-background)',
                text: 'var(--color-text)',
                textMuted: 'var(--color-text-muted)',
                accent: 'var(--color-accent)',
                danger: 'var(--color-danger)',
                success: 'var(--color-success)',
                slate: { 950: '#020617' },
                cyan: { 400: '#22d3ee', 500: '#06b6d4' },
                pink: { 500: '#ec4899' },
                emerald: { 400: '#34d399' },
            },
            fontFamily: {
                sans: ['Inter', 'system-ui', '-apple-system', 'Segoe UI', 'Roboto', 'sans-serif'],
                mono: ['JetBrains Mono', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
            },
        },
    },
    plugins: [],
}
