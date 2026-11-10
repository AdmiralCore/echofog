// The Settings card that chooses the protocol the node runs on (T11.7). The default stays 1 until T15.5.
export default {
  en: {
    'protocol.title': 'Protocol version',
    'protocol.hint': 'Version 2 is the new network with its own keys and data folder. The choice applies after EchoFog is restarted; the current profile keeps working in version 1 until then.',
    'protocol.v1': 'Version 1',
    'protocol.v2': 'Version 2 (new network, testing)',
    'protocol.running': 'Running now: version {n}',
    'protocol.now': 'Running now',
    'protocol.pending': 'After restart: version {n}',
    'protocol.warn2': 'Version 2 is still being tested. Contacts and chats of version 1 are not moved to it.',
    'protocol.saved': 'Saved. Restart EchoFog to switch.',
  },
  ru: {
    'protocol.title': 'Версия протокола',
    'protocol.hint': 'Версия 2 — новая сеть со своими ключами и своей папкой данных. Выбор применяется после перезапуска EchoFog; до этого профиль работает в версии 1.',
    'protocol.v1': 'Версия 1',
    'protocol.v2': 'Версия 2 (новая сеть, проверка)',
    'protocol.running': 'Сейчас работает версия {n}',
    'protocol.now': 'Работает сейчас',
    'protocol.pending': 'После перезапуска: версия {n}',
    'protocol.warn2': 'Версия 2 ещё проверяется. Контакты и чаты версии 1 в неё не переносятся.',
    'protocol.saved': 'Сохранено. Для переключения перезапустите EchoFog.',
  },
};
