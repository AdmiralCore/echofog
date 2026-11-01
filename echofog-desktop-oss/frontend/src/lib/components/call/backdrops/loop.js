import { readable } from 'svelte/store';

// One frame loop for every animated tile backdrop. 30 frames a second is plenty for a background
// (the screen may run at 144), and the loop stops when no backdrop is on screen.
const FPS = 30;
const running = new Set();
let raf = 0;
let last = 0;

function frame(now) {
    raf = running.size ? requestAnimationFrame(frame) : 0;
    const dt = (now - last) / 1000;
    if (dt < 1 / FPS - 0.004) return;
    last = now;
    for (const fn of running) fn(now / 1000, Math.min(dt, 0.1));
}

/** animate calls fn(t, dt) on every frame (seconds) until the returned function is called */
export function animate(fn) {
    running.add(fn);
    if (!raf) {
        last = performance.now();
        raf = requestAnimationFrame(frame);
    }
    return () => running.delete(fn);
}

/** the system asks for less motion: backdrops show one still frame */
export const reducedMotion = readable(false, (set) => {
    const mq = typeof window !== 'undefined' && window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;
    if (!mq) return undefined;
    set(mq.matches);
    const changed = (e) => set(e.matches);
    mq.addEventListener('change', changed);
    return () => mq.removeEventListener('change', changed);
});
