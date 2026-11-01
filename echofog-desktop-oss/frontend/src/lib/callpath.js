// What the call's relay badge and panel say for the path the core reports (GetCallPath, see
// core-oss pkg/api/node/callpath.go): the reason codes come from there.

/** reasons the relay is used on purpose: said plainly, with no advice */
export const CHOSEN = ['anonymous', 'proxy', 'settings', 'guard'];

// the steps that help, in order; none where the relay is chosen, where the fix is on the other
// side, or while a direct link is still being looked for
const FIXES = {
  cgnat: ['white', 'relay'],
  router: ['upnp', 'forward', 'cgnatNote'],
};

export const fixesFor = (reason) => FIXES[reason] || [];
export const chosen = (reason) => CHOSEN.includes(reason);

/** The badge: hidden (null) unless somebody is reached through a relay. */
export function badgeOf(path) {
  if (!path?.inCall || !path.relayed) return null;
  const total = path.members.length;
  const warn = !!path.hurts;
  if (total > 1) return { key: 'callpath.badgeGroup', params: { n: path.relayed, total }, warn };
  if (path.reason === 'anonymous') return { key: 'callpath.badgeAnon', params: {}, warn };
  if (path.reason === 'settings' || path.reason === 'proxy') return { key: 'callpath.badgeChosen', params: {}, warn };
  if (path.pingMs) return { key: 'callpath.badgePing', params: { ms: path.pingMs }, warn };
  return { key: 'callpath.badge', params: {}, warn };
}

/** The panel's first line. */
export function leadOf(path) {
  if (path.members.length > 1) return { key: 'callpath.lead.group', params: { n: path.relayed, total: path.members.length } };
  if (chosen(path.reason)) return { key: 'callpath.lead.chosen', params: {} };
  if (path.reason === 'trying') return { key: 'callpath.lead.trying', params: {} };
  return { key: 'callpath.lead.failed', params: {} };
}

/** This side's NAT, as one of the callpath.nat.* words. */
export function mySide(path) {
  if (path.carrierNat) return 'cgnat';
  if (path.mapped) return 'mapped';
  return ['public', 'cone', 'symmetric'].includes(path.nat) ? path.nat : 'unknown';
}

/** host:port of a relay address (a ws:// URL stays whole); the short ID when there is none. */
export function relayHost(addr, peerId) {
  if (!addr) return peerId ? peerId.slice(0, 6) + '…' + peerId.slice(-4) : '';
  return addr.replace(/\/[0-9a-f]{64}$/i, '');
}
