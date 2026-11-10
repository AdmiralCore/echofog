// Panels over video: shown while the pointer is over the node and for `ms` after it last moved,
// or while something inside has keyboard focus or an open menu pins them. The node carries
// data-active="true|false"; CSS does the fading (opacity only: no filters over the picture).
export function autohide(node, opts = {}) {
    let ms = opts.ms ?? 3000;
    let timer = 0;
    let inside = false;

    const set = (on) => {
        node.dataset.active = on ? 'true' : 'false';
    };
    const arm = () => {
        clearTimeout(timer);
        timer = setTimeout(() => {
            // keyboard focus inside keeps the panels
            if (node.matches(':focus-within') && node.querySelector(':focus-visible')) return arm();
            set(false);
        }, ms);
    };
    const wake = () => {
        set(true);
        arm();
    };
    const enter = () => {
        inside = true;
        wake();
    };
    const leave = () => {
        inside = false;
        clearTimeout(timer);
        timer = setTimeout(() => !inside && set(false), 600);
    };
    const key = () => wake();

    set(false);
    node.addEventListener('pointermove', wake, { passive: true });
    node.addEventListener('pointerenter', enter, { passive: true });
    node.addEventListener('pointerleave', leave, { passive: true });
    node.addEventListener('focusin', key);
    node.addEventListener('keydown', key);
    return {
        update(o = {}) {
            ms = o.ms ?? 3000;
        },
        destroy() {
            clearTimeout(timer);
            node.removeEventListener('pointermove', wake);
            node.removeEventListener('pointerenter', enter);
            node.removeEventListener('pointerleave', leave);
            node.removeEventListener('focusin', key);
            node.removeEventListener('keydown', key);
        },
    };
}
