import { writable } from 'svelte/store';

export const THEMES = ['super-dark', 'super-light'];

function loadTheme() {
    try {
        const saved = localStorage.getItem('echofog_theme');
        if (THEMES.includes(saved)) return saved;
    } catch (_) { /* storage unavailable */ }
    return 'super-dark';
}

export const currentTheme = writable(loadTheme());

currentTheme.subscribe((theme) => {
    if (typeof document === 'undefined') return;
    document.body.className = `theme-${theme}`;
    document.documentElement.style.colorScheme = theme === 'super-light' ? 'light' : 'dark';
    try { localStorage.setItem('echofog_theme', theme); } catch (_) { /* ignore */ }
});

export function toggleTheme() {
    currentTheme.update((c) => (c === 'super-dark' ? 'super-light' : 'super-dark'));
}

export const currentBackground = writable({ type: 'color', color: '#000000' });
