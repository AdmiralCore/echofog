import { tr, intlLocale } from './i18n/index.js';

export function bytes(n) {
  const u = tr('unit.bytes').split(',');
  if (!n) return '0 ' + u[0];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i === 0 ? 0 : 1) + ' ' + u[i];
}

// Exit prices travel as micro-USDT per MB (49 is about 5 cents a GB) and are shown per GB.
export const usdPerGb = (pricePerMb) => (pricePerMb * 1024) / 1e6;
export const microPerMb = (usd) => Math.max(0, Math.round(((Number(usd) || 0) * 1e6) / 1024));
export function price(pricePerMb) {
  if (!pricePerMb) return tr('common.free');
  const v = usdPerGb(pricePerMb);
  return '$' + v.toFixed(v < 0.1 ? 3 : 2) + tr('unit.perGb');
}

export function amount(v) {
  const n = Number(v);
  if (!isFinite(n)) return String(v);
  return n.toLocaleString(intlLocale(), { maximumFractionDigits: n < 1 ? 4 : 2 });
}

// ms accepts seconds or milliseconds.
/** Milliseconds from a time in seconds, milliseconds, microseconds or nanoseconds. */
export function toMs(ts) {
  if (!ts) return ts;
  if (ts < 1e12) return ts * 1000; // seconds
  if (ts < 1e15) return ts; // milliseconds
  if (ts < 1e18) return Math.floor(ts / 1e3); // microseconds
  return Math.floor(ts / 1e6); // nanoseconds
}

export function time(ts) {
  return new Date(toMs(ts)).toLocaleTimeString(intlLocale(), { hour: '2-digit', minute: '2-digit' });
}

export function when(ts) {
  const d = new Date(toMs(ts));
  return new Date().toDateString() === d.toDateString()
    ? time(ts)
    : d.toLocaleDateString(intlLocale(), { day: 'numeric', month: 'short' });
}

export function short(id, n = 6) {
  return id ? `${id.slice(0, n)}…${id.slice(-n)}` : '';
}

export function duration(seconds) {
  const h = Math.floor(seconds / 3600);
  const m = String(Math.floor((seconds % 3600) / 60)).padStart(2, '0');
  const s = String(seconds % 60).padStart(2, '0');
  return h ? `${h}:${m}:${s}` : `${m}:${s}`;
}
