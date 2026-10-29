// Permission bits of a group's roles, as the core has them (convlog.Perm*, groups.Perm*).
export const PERM = {
  send: 1 << 0,
  media: 1 << 1,
  voice: 1 << 2,
  stream: 1 << 3,
  delete: 1 << 4,
  kick: 1 << 5,
  ban: 1 << 6,
  roles: 1 << 7,
  group: 1 << 8,
  links: 1 << 9,
  threads: 1 << 10,
  pin: 1 << 11,
  react: 1 << 12,
  invite: 1 << 13,
  channels: 1 << 14,
};

/** The permissions as the role editor groups them. */
export const PERM_GROUPS = [
  { key: 'messages', perms: ['send', 'media', 'links', 'threads', 'react', 'pin'] },
  { key: 'voice', perms: ['voice', 'stream'] },
  { key: 'moderation', perms: ['delete', 'kick', 'ban'] },
  { key: 'management', perms: ['invite', 'channels', 'roles', 'group'] },
];

/** Colours offered for a role (0: none). */
export const ROLE_COLORS = [0, 0xe5484d, 0xf76b15, 0xffc53d, 0x46a758, 0x12a594, 0x3e63dd, 0x8e4ec6, 0xd6409f, 0x8b8d98];

export const hexColor = (c) => (c ? '#' + c.toString(16).padStart(6, '0') : '');
