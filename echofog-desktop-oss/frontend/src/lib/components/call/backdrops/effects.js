// Animated backgrounds for call tiles without a camera, drawn on a 2D canvas. Each effect is
// make(ctx, w, h, color) -> draw(t, dt, energy): w and h are canvas pixels, color is the person's
// '#rrggbb', t and dt are seconds and energy (0..1) rises while the person speaks. `scale` is the
// canvas resolution against the tile's CSS size: soft effects are drawn small and stretched.
// A frame costs well under a millisecond at 480x270 (cubes and particles are the dearest).
// Everything a frame needs is prepared in make(); a frame only moves things and fills paths.

const BASE = [22, 23, 27];
const WHITE = [255, 255, 255];

function rgbOf(hex) {
    const n = parseInt(String(hex).replace('#', ''), 16);
    return Number.isFinite(n) ? [(n >> 16) & 255, (n >> 8) & 255, n & 255] : [101, 170, 221];
}

const css = ([r, g, b], a = 1) => `rgba(${r | 0},${g | 0},${b | 0},${a})`;
const mix = (c, d, k) => c.map((v, i) => v + (d[i] - v) * k);

// hue rotation about the grey axis: companion colours for a person's own
function hue(c, deg) {
    const a = (deg * Math.PI) / 180;
    const cos = Math.cos(a);
    const s = Math.sqrt(1 / 3) * Math.sin(a);
    const k = (1 - cos) / 3;
    const [r, g, b] = c;
    return [
        r * (cos + k) + g * (k - s) + b * (k + s),
        r * (k + s) + g * (cos + k) + b * (k - s),
        r * (k - s) + g * (k + s) + b * (cos + k),
    ].map((v) => Math.max(0, Math.min(255, v)));
}

function vertical(ctx, h, top, bottom) {
    const g = ctx.createLinearGradient(0, 0, 0, h);
    g.addColorStop(0, top);
    g.addColorStop(1, bottom);
    return g;
}

// Soft gradient: three blurred lights in the person's colours drift slowly over each other.
function gradient(ctx, w, h, color) {
    const c = rgbOf(color);
    const lights = [c, hue(c, 50), hue(c, -60)].map((col, i) => ({ col, ph: i * 2.1, sx: 0.22 + i * 0.08, sy: 0.18 + i * 0.1 }));
    const R = Math.max(w, h);
    const base = css(BASE);
    return (t, _dt, e) => {
        ctx.fillStyle = base;
        ctx.fillRect(0, 0, w, h);
        for (const l of lights) {
            const x = w * (0.5 + 0.38 * Math.sin(t * l.sx + l.ph));
            const y = h * (0.5 + 0.36 * Math.cos(t * l.sy + l.ph * 1.3));
            const r = R * (0.55 + 0.12 * Math.sin(t * 0.3 + l.ph));
            const g = ctx.createRadialGradient(x, y, 0, x, y, r);
            g.addColorStop(0, css(l.col, 0.42 + 0.25 * e));
            g.addColorStop(1, css(l.col, 0));
            ctx.fillStyle = g;
            ctx.fillRect(0, 0, w, h);
        }
    };
}

// Waves: four layers of swell, the front ones with a light crest; speaking makes them higher.
function waves(ctx, w, h, color) {
    const c = rgbOf(color);
    const bg = vertical(ctx, h, css(mix(BASE, c, 0.1)), css(BASE));
    const step = Math.max(3, w / 64);
    const layers = [0, 1, 2, 3].map((i) => {
        const col = hue(c, (i - 1.5) * 18);
        const top = h * (0.4 + i * 0.13);
        return {
            top,
            amp: h * (0.05 + 0.012 * i),
            k1: (Math.PI * 2) / (w * (1.1 - i * 0.15)),
            k2: (Math.PI * 2) / (w * 0.45),
            s1: 0.45 + i * 0.15,
            s2: -0.3 - i * 0.05,
            ph: i * 1.7,
            fill: vertical(ctx, h, css(col, 0), css(col, 0.16 + i * 0.07)),
            crest: i >= 2 ? css(mix(col, WHITE, 0.45), 0.35) : '',
        };
    });
    const ys = new Float32Array(Math.ceil(w / step) + 2);
    return (t, _dt, e) => {
        ctx.fillStyle = bg;
        ctx.fillRect(0, 0, w, h);
        for (const L of layers) {
            const amp = L.amp * (1 + 0.9 * e);
            let n = 0;
            for (let x = 0; x <= w + step; x += step) {
                ys[n++] = L.top + amp * (0.7 * Math.sin(x * L.k1 + t * L.s1 + L.ph) + 0.3 * Math.sin(x * L.k2 + t * L.s2 + L.ph * 2));
            }
            ctx.beginPath();
            ctx.moveTo(0, h);
            for (let i = 0; i < n; i++) ctx.lineTo(i * step, ys[i]);
            ctx.lineTo(w, h);
            ctx.closePath();
            ctx.fillStyle = L.fill;
            ctx.fill();
            if (L.crest) {
                ctx.beginPath();
                ctx.moveTo(0, ys[0]);
                for (let i = 1; i < n; i++) ctx.lineTo(i * step, ys[i]);
                ctx.strokeStyle = L.crest;
                ctx.lineWidth = Math.max(1, h / 160);
                ctx.stroke();
            }
        }
    };
}

// Particles: points drift and link up with their neighbours, like a constellation.
function particles(ctx, w, h, color) {
    const c = rgbOf(color);
    const unit = Math.min(w, h);
    const n = 22;
    const pts = Array.from({ length: n }, () => {
        const a = Math.random() * Math.PI * 2;
        const v = unit * (0.02 + Math.random() * 0.05);
        return { x: Math.random() * w, y: Math.random() * h, vx: Math.cos(a) * v, vy: Math.sin(a) * v, r: unit * (0.008 + Math.random() * 0.012) };
    });
    const D = unit * 0.32;
    const D2 = D * D;
    const m = 8;
    const bg = vertical(ctx, h, css(mix(BASE, c, 0.08)), css(BASE));
    const dot = css(mix(c, WHITE, 0.35), 0.9);
    const LINES = [0.1, 0.2, 0.32, 0.48];
    const strokes = LINES.map((a) => css(c, a));
    const buckets = LINES.map(() => []);
    return (t, dt, e) => {
        const sp = dt * (1 + 2.2 * e);
        for (const p of pts) {
            p.x += p.vx * sp;
            p.y += p.vy * sp;
            if (p.x < -m) p.x += w + 2 * m;
            else if (p.x > w + m) p.x -= w + 2 * m;
            if (p.y < -m) p.y += h + 2 * m;
            else if (p.y > h + m) p.y -= h + 2 * m;
        }
        ctx.fillStyle = bg;
        ctx.fillRect(0, 0, w, h);
        for (const b of buckets) b.length = 0;
        for (let i = 0; i < n; i++) {
            const a = pts[i];
            for (let j = i + 1; j < n; j++) {
                const b = pts[j];
                const dx = a.x - b.x;
                const dy = a.y - b.y;
                const d2 = dx * dx + dy * dy;
                if (d2 >= D2) continue;
                const k = 1 - Math.sqrt(d2) / D;
                buckets[Math.min(3, (k * 4) | 0)].push(a.x, a.y, b.x, b.y);
            }
        }
        ctx.lineWidth = Math.max(1, unit / 220);
        ctx.globalAlpha = 0.75 + 0.25 * e;
        buckets.forEach((b, i) => {
            if (!b.length) return;
            ctx.beginPath();
            for (let k = 0; k < b.length; k += 4) {
                ctx.moveTo(b[k], b[k + 1]);
                ctx.lineTo(b[k + 2], b[k + 3]);
            }
            ctx.strokeStyle = strokes[i];
            ctx.stroke();
        });
        ctx.globalAlpha = 1;
        ctx.fillStyle = dot;
        ctx.beginPath();
        for (const p of pts) {
            ctx.moveTo(p.x + p.r, p.y);
            ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2);
        }
        ctx.fill();
    };
}

// Stars: a slow flight through a starfield lit by a nebula of the person's colour; speaking
// speeds it up into streaks.
function stars(ctx, w, h, color) {
    const c = rgbOf(color);
    const n = Math.round(Math.min(160, Math.max(50, (w * h) / 1500)));
    const cx = w / 2;
    const cy = h / 2;
    const spawn = (s, far) => {
        s.x = Math.random() * 2 - 1;
        s.y = Math.random() * 2 - 1;
        s.z = far ? 1 : 0.1 + Math.random() * 0.9;
        return s;
    };
    const list = Array.from({ length: n }, () => spawn({}, false));
    const nebula = [
        [w * 0.3, h * 0.35, c],
        [w * 0.72, h * 0.65, hue(c, 70)],
    ].map(([x, y, col]) => {
        const g = ctx.createRadialGradient(x, y, 0, x, y, Math.max(w, h) * 0.55);
        g.addColorStop(0, css(col, 0.22));
        g.addColorStop(1, css(col, 0));
        return g;
    });
    const base = css([9, 10, 14]);
    const tint = mix(c, WHITE, 0.75);
    const shades = [0.25, 0.45, 0.7, 1].map((a) => css(tint, a));
    const buckets = shades.map(() => []);
    const px = (s, z) => cx + (s.x * w * 0.3) / z;
    const py = (s, z) => cy + (s.y * h * 0.3) / z;
    const width = Math.max(1, Math.min(w, h) / 200);
    return (t, dt, e) => {
        const v = 0.05 + 0.55 * e;
        ctx.fillStyle = base;
        ctx.fillRect(0, 0, w, h);
        for (const g of nebula) {
            ctx.fillStyle = g;
            ctx.fillRect(0, 0, w, h);
        }
        for (const b of buckets) b.length = 0;
        for (const s of list) {
            s.z -= v * dt;
            let x = px(s, s.z);
            let y = py(s, s.z);
            if (s.z < 0.05 || x < 0 || x > w || y < 0 || y > h) {
                spawn(s, true);
                x = px(s, s.z);
                y = py(s, s.z);
            }
            const tail = s.z + v * 0.12 + 0.004;
            buckets[Math.min(3, ((1 - s.z) * 4) | 0)].push(px(s, tail), py(s, tail), x, y);
        }
        ctx.lineCap = 'round';
        buckets.forEach((b, i) => {
            if (!b.length) return;
            ctx.beginPath();
            for (let k = 0; k < b.length; k += 4) {
                ctx.moveTo(b[k], b[k + 1]);
                ctx.lineTo(b[k + 2], b[k + 3]);
            }
            ctx.strokeStyle = shades[i];
            ctx.lineWidth = width * (0.8 + i * 0.45);
            ctx.stroke();
        });
    };
}

// Cubes: an isometric field of blocks with a ripple running out from the middle; the higher a
// block, the brighter its top. Speaking makes the ripple taller. Each block is one drawImage of
// a block drawn once per brightness level: paths for every face cost several times more.
function cubes(ctx, w, h, color) {
    const c = rgbOf(color);
    const s = Math.max(8, h / 4.2);
    const a = s * 0.866;
    const b = s * 0.5;
    const cx = w / 2;
    const cy = h / 2;
    const LEVELS = 8;
    const AMP = 1.25; // the tallest ripple, in blocks
    // a side shows down to where the blocks in front cover it
    const depth = b + s * AMP + 2;
    const sprites = Array.from({ length: LEVELS }, (_, l) => {
        const sw = Math.ceil(2 * a) + 2;
        const sh = Math.ceil(2 * b + depth) + 2;
        const sp = typeof OffscreenCanvas === 'function' ? new OffscreenCanvas(sw, sh) : Object.assign(document.createElement('canvas'), { width: sw, height: sh });
        const g = sp.getContext('2d');
        const x = a + 1;
        const y = b + 1;
        const face = (fill, pts) => {
            g.fillStyle = fill;
            g.beginPath();
            g.moveTo(pts[0], pts[1]);
            for (let i = 2; i < pts.length; i += 2) g.lineTo(pts[i], pts[i + 1]);
            g.closePath();
            g.fill();
        };
        const k = l / (LEVELS - 1);
        // sides fade into the dark below, the top is lit with a light rim
        const side = (amount) => {
            const gr = g.createLinearGradient(0, y, 0, y + b + depth);
            gr.addColorStop(0, css(mix(BASE, c, amount)));
            gr.addColorStop(1, css(BASE));
            return gr;
        };
        face(side(0.2 + 0.2 * k), [x - a, y, x, y + b, x, y + b + depth, x - a, y + depth]);
        face(side(0.08 + 0.1 * k), [x + a, y, x, y + b, x, y + b + depth, x + a, y + depth]);
        face(css(mix(BASE, c, 0.22 + 0.5 * k)), [x, y - b, x + a, y, x, y + b, x - a, y]);
        g.beginPath();
        g.moveTo(x - a + 1, y);
        g.lineTo(x, y - b + 0.5);
        g.lineTo(x + a - 1, y);
        g.strokeStyle = css(mix(c, WHITE, 0.6), 0.25 + 0.35 * k);
        g.lineWidth = 1;
        g.stroke();
        // a bitmap is a ready texture; a canvas source is snapshotted on every draw
        return sp.transferToImageBitmap ? sp.transferToImageBitmap() : sp;
    });
    // the blocks that can show, back to front
    const cells = [];
    const N = Math.ceil(Math.max(w / a, h / b) / 2) + 2;
    for (let i = -N; i <= N; i++) {
        for (let j = -N; j <= N; j++) {
            const x = cx + (i - j) * a;
            const y = cy + (i + j) * b;
            if (x < -a || x > w + a || y < -(b + depth) || y > h + s * AMP + b) continue;
            cells.push({ x: x - a - 1, y: y - b - 1, d: Math.hypot(i, j), row: i + j });
        }
    }
    cells.sort((p, q) => p.row - q.row);
    const base = vertical(ctx, h, css(mix(BASE, c, 0.08)), css(BASE));
    return (t, _dt, e) => {
        ctx.fillStyle = base;
        ctx.fillRect(0, 0, w, h);
        const amp = s * AMP * (0.4 + 0.6 * e);
        for (const p of cells) {
            const k = 0.5 + 0.5 * Math.sin(t * 1.1 - p.d * 0.6);
            ctx.drawImage(sprites[Math.min(LEVELS - 1, (k * LEVELS) | 0)], p.x, p.y - amp * k);
        }
    };
}

// Matrix: columns of falling glyphs leaving green trails; speaking makes them rain faster.
const GLYPHS = 'ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖﾗﾘﾙﾚﾛﾜﾝ0123456789Z:.=*+-<>¦';
const TRAIL = ['#1f9d55', '#27c26a', '#35e07f'];

function matrix(ctx, w, h) {
    const size = Math.max(9, Math.round(h / 15));
    const cols = Math.ceil(w / size);
    const rows = Math.ceil(h / size);
    const pick = () => GLYPHS[(Math.random() * GLYPHS.length) | 0];
    const drop = (d, top) => {
        d.y = top ? -Math.random() * 4 : -Math.random() * rows;
        d.row = Math.floor(d.y);
        d.v = 6 + Math.random() * 10;
        d.head = '';
        return d;
    };
    const drops = Array.from({ length: cols }, () => drop({}, false));
    ctx.fillStyle = '#0a0d0b';
    ctx.fillRect(0, 0, w, h);
    ctx.font = `${size}px "MS Gothic", "Yu Gothic", monospace`;
    ctx.textBaseline = 'top';
    const glyph = (ch, i, r, fill) => {
        ctx.fillStyle = fill;
        ctx.fillText(ch, i * size, r * size);
    };
    const draw = (t, dt, e) => {
        if (!dt) return;
        ctx.fillStyle = `rgba(10,13,11,${1 - Math.pow(0.9, dt * 30)})`;
        ctx.fillRect(0, 0, w, h);
        for (let i = 0; i < cols; i++) {
            const d = drops[i];
            d.y += d.v * dt * (1 + 1.2 * e);
            const row = Math.floor(d.y);
            if (row === d.row) continue;
            if (d.head) glyph(d.head, i, d.row, TRAIL[i % 3]);
            for (let r = Math.max(0, d.row + 1); r < row; r++) glyph(pick(), i, r, TRAIL[(i + r) % 3]);
            d.row = row;
            d.head = '';
            if (row > rows) {
                if (Math.random() < 0.1) drop(d, true);
                continue;
            }
            if (row >= 0) {
                d.head = pick();
                glyph(d.head, i, row, e > 0.5 ? '#ffffff' : '#d6ffe4');
            }
        }
    };
    // start mid-rain, not on an empty screen
    for (let k = 0; k < 60; k++) draw(0, 1 / 30, 0);
    return draw;
}

// `max`: the most canvas pixels an effect gets on a big tile, which then stretches it
const SD = 640 * 360;
export const EFFECTS = {
    gradient: { scale: 0.2, max: SD, make: gradient },
    waves: { scale: 0.5, max: SD, make: waves },
    particles: { scale: 1, max: SD, make: particles },
    stars: { scale: 1, max: SD, make: stars },
    cubes: { scale: 0.75, max: 480 * 270, make: cubes },
    matrix: { scale: 1, max: 1280 * 720, make: matrix },
};
