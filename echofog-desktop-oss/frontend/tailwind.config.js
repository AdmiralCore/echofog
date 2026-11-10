/** @type {import('tailwindcss').Config} */
// Every value points at a CSS variable from src/app.css, so both themes and the density switch
// work through Tailwind classes too. New names are added next to Tailwind's defaults (not over
// them) so screens written before the redesign keep their sizes; names under "legacy" are theirs.
export default {
    content: ["./index.html", "./src/**/*.{svelte,js,ts,jsx,tsx}"],
    theme: {
        extend: {
            colors: {
                // surfaces and text
                canvas: 'var(--bg)',
                s1: 'var(--surface-1)',
                s2: 'var(--surface-2)',
                s3: 'var(--surface-3)',
                stage: 'var(--stage)',
                tile: 'var(--tile)',
                line: 'var(--border)',
                'line-strong': 'var(--border-strong)',
                fg: 'var(--text)',
                'fg-muted': 'var(--muted)',
                'fg-faint': 'var(--faint)',
                // accent and status
                'accent-fg': 'var(--accent-fg)',
                'accent-ink': 'var(--accent-ink)',
                'accent-soft': 'var(--accent-soft)',
                ok: 'var(--ok)',
                'ok-soft': 'var(--ok-soft)',
                warn: 'var(--warn)',
                'warn-soft': 'var(--warn-soft)',
                'danger-soft': 'var(--danger-soft)',
                live: 'var(--live)',
                info: 'var(--info)',
                'bubble-in': 'var(--bubble-in)',
                'bubble-out': 'var(--bubble-out)',
                // legacy
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
                sans: ['Inter', 'Segoe UI Variable Text', 'Segoe UI', 'system-ui', '-apple-system', 'Roboto', 'sans-serif'],
                mono: ['Cascadia Code', 'Cascadia Mono', 'JetBrains Mono', 'ui-monospace', 'Consolas', 'monospace'],
            },
            // text-t-md etc.: the design scale (size + line height)
            fontSize: {
                't-2xs': ['var(--fs-2xs)', 'var(--lh-2xs)'],
                't-xs': ['var(--fs-xs)', 'var(--lh-xs)'],
                't-sm': ['var(--fs-sm)', 'var(--lh-sm)'],
                't-md': ['var(--fs-md)', 'var(--lh-md)'],
                't-chat': ['var(--fs-chat)', 'var(--lh-chat)'],
                't-lg': ['var(--fs-lg)', 'var(--lh-lg)'],
                't-xl': ['var(--fs-xl)', 'var(--lh-xl)'],
                't-2xl': ['var(--fs-2xl)', 'var(--lh-2xl)'],
            },
            // rounded-ctl (buttons, fields) / menu / card / bubble
            borderRadius: {
                tag: 'var(--r-sm)',
                ctl: 'var(--r-md)',
                menu: 'var(--r-lg)',
                card: 'var(--r-xl)',
                bubble: 'var(--r-bubble)',
                'bubble-s': 'var(--r-bubble-s)',
            },
            boxShadow: {
                'e-sm': 'var(--shadow-sm)',
                'e-md': 'var(--shadow-md)',
                'e-lg': 'var(--shadow-lg)',
                'e-xl': 'var(--shadow-xl)',
            },
            transitionDuration: {
                fast: 'var(--dur-fast)',
                ui: 'var(--dur)',
                slow: 'var(--dur-slow)',
            },
            transitionTimingFunction: {
                ui: 'var(--ease)',
            },
            height: { 'ctl-sm': 'var(--ctl-sm)', 'ctl-md': 'var(--ctl-md)', 'ctl-lg': 'var(--ctl-lg)' },
        },
    },
    plugins: [],
}
