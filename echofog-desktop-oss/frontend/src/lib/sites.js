// Services people check most often through a proxy. A tile shows a monogram in the brand colour.
export const PRESET_SITES = [
  { name: 'YouTube', host: 'www.youtube.com', mark: 'YT', color: '#ff2a4a' },
  { name: 'Discord', host: 'discord.com', mark: 'D', color: '#5865f2' },
  { name: 'Instagram', host: 'www.instagram.com', mark: 'IG', color: '#e1306c' },
  { name: 'Facebook', host: 'www.facebook.com', mark: 'f', color: '#1877f2' },
  { name: 'X', host: 'x.com', mark: 'X', color: '#8b98a5' },
  { name: 'WhatsApp', host: 'web.whatsapp.com', mark: 'WA', color: '#25d366' },
  { name: 'Telegram', host: 'web.telegram.org', mark: 'TG', color: '#29a9eb' },
  { name: 'Signal', host: 'signal.org', mark: 'S', color: '#3a76f0' },
  { name: 'ChatGPT', host: 'chatgpt.com', mark: 'AI', color: '#10a37f' },
  { name: 'Spotify', host: 'open.spotify.com', mark: 'Sp', color: '#1ed760' },
  { name: 'LinkedIn', host: 'www.linkedin.com', mark: 'in', color: '#0a66c2' },
  { name: 'Viber', host: 'www.viber.com', mark: 'V', color: '#7360f2' },
  { name: 'Twitch', host: 'www.twitch.tv', mark: 'Tw', color: '#9146ff' },
  { name: 'SoundCloud', host: 'soundcloud.com', mark: 'SC', color: '#ff5500' },
  { name: 'Medium', host: 'medium.com', mark: 'M', color: '#a8a8a8' },
  { name: 'Wikipedia', host: 'www.wikipedia.org', mark: 'W', color: '#c8ccd1' },
];

/** The most sites used to rank exits (the core keeps at most 16). */
export const MAX_SELECTED = 16;

/** host:port for a typed site: "youtube.com", "https://www.youtube.com/watch?v=1", "host:8443". */
export function siteTarget(input) {
  let s = String(input || '').trim().toLowerCase();
  s = s.replace(/^https?:\/\//, '').split(/[/?#]/)[0];
  if (!s || /[\s@\\]/.test(s) || !/^[a-z0-9.-]+(:\d{1,5})?$/.test(s) || !s.includes('.')) return '';
  return s.includes(':') ? s : s + ':443';
}

export const hostOf = (target) => target.replace(/:443$/, '');

const CUSTOM = 'echofog.sites.custom';

/** Sites the user added: [{ host, name }] */
export function loadCustom() {
  try {
    const v = JSON.parse(localStorage.getItem(CUSTOM) || '[]');
    return Array.isArray(v) ? v.filter((x) => x && x.host) : [];
  } catch (_) {
    return [];
  }
}

export function saveCustom(list) {
  try { localStorage.setItem(CUSTOM, JSON.stringify(list)); } catch (_) { /* private mode */ }
}
