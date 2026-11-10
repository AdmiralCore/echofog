<script>
  // Component gallery for `npm run dev` + `?mock&gallery`. Not part of the real build (see main.js).
  import { t } from '../i18n/index.js';
  import { theme } from '../stores/theme.js';
  import { ask } from '../stores/dialog.js';
  import { toast } from '../stores/toast.js';
  import { feedMode, COMPACT_ABOVE } from '../stores/feed.js';
  import { ICONS } from '../icons.js';
  import Icon from '../components/ui/Icon.svelte';
  import Button from '../components/ui/Button.svelte';
  import IconButton from '../components/ui/IconButton.svelte';
  import Input from '../components/ui/Input.svelte';
  import Textarea from '../components/ui/Textarea.svelte';
  import Select from '../components/ui/Select.svelte';
  import Toggle from '../components/ui/Toggle.svelte';
  import Checkbox from '../components/ui/Checkbox.svelte';
  import Tabs from '../components/ui/Tabs.svelte';
  import Badge from '../components/ui/Badge.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import Tooltip from '../components/ui/Tooltip.svelte';
  import Menu from '../components/ui/Menu.svelte';
  import Modal from '../components/ui/Modal.svelte';
  import Dialog from '../components/ui/Dialog.svelte';
  import Toast from '../components/ui/Toast.svelte';
  import Card from '../components/ui/Card.svelte';
  import ListItem from '../components/ui/ListItem.svelte';
  import Divider from '../components/ui/Divider.svelte';
  import Skeleton from '../components/ui/Skeleton.svelte';
  import ScrollArea from '../components/ui/ScrollArea.svelte';

  let density = 'comfortable';
  $: document.documentElement.dataset.density = density;

  const sections = ['tokens', 'icons', 'buttons', 'fields', 'nav', 'people', 'overlay', 'surfaces', 'feed', 'room'];

  const swatches = {
    surfaces: ['bg', 'surface-1', 'surface-2', 'surface-3', 'tile', 'border', 'border-strong', 'hover'],
    text: ['text', 'muted', 'faint'],
    status: ['accent', 'accent-hover', 'accent-soft', 'ok', 'warn', 'danger', 'live', 'info', 'bubble-in', 'bubble-out'],
  };
  const typeScale = [['2xl', 'fw-semibold'], ['xl', 'fw-semibold'], ['lg', 'fw-semibold'], ['chat', 'fw-regular'], ['md', 'fw-regular'], ['sm', 'fw-regular'], ['xs', 'fw-regular'], ['2xs', 'fw-medium']];
  const spaces = [1, 2, 3, 4, 5, 6, 8, 10, 12];
  const radii = [['xs', '4'], ['sm', '6'], ['md', '8'], ['lg', '10'], ['xl', '12'], ['bubble-s', '14'], ['bubble', '18']];

  // fields
  let name = 'Alice Moreau';
  let nameBad = 'alice';
  let about = '';
  let lang = 'ru';
  let tgNotify = true;
  let tgSound = false;
  let cbA = true;
  let cbB = false;
  let tab = 'chats';
  let segTab = 'dark';
  let picked = '';

  $: tabItems = [
    { id: 'chats', label: $t('g.tab.chats'), icon: 'message', count: 3 },
    { id: 'files', label: $t('g.tab.files'), icon: 'file' },
    { id: 'members', label: $t('g.tab.members'), icon: 'users', count: 128 },
    { id: 'off', label: $t('g.tab.off'), disabled: true },
  ];
  $: menuItems = [
    { id: 'reply', label: $t('g.menu.reply'), icon: 'reply', hint: 'R' },
    { id: 'copy', label: $t('g.menu.copy'), icon: 'copy', hint: 'Ctrl C' },
    { id: 'pin', label: $t('g.menu.pin'), icon: 'pin' },
    { separator: true },
    { id: 'delete', label: $t('g.menu.delete'), icon: 'trash', danger: true },
  ];
  $: langOptions = [{ value: 'ru', label: 'Русский' }, { value: 'en', label: 'English' }];

  let modalOpen = false;

  // feed
  let feedPrefLocal = 'auto';
  let feedMembers = 8;
  $: shownMode = feedMode(feedPrefLocal, feedMembers);
  $: feedPrefItems = [
    { id: 'auto', label: $t('g.feed.auto') },
    { id: 'bubbles', label: $t('g.feed.bubbles') },
    { id: 'compact', label: $t('g.feed.compact') },
  ];

  // room: a 1440 px window, modelled in numbers so the readout is exact
  const WIN = 1440;
  let roomSide = true;
  let roomStrip = true;
  let roomChat = false;
  let theatre = false;
  $: sideW = theatre || !roomSide ? 0 : 272;
  $: partW = roomStrip ? 56 : 176;
  $: chatW = theatre || !roomChat ? 0 : 300;
  $: videoW = WIN - sideW - partW - chatW;
  function onKey(e) {
    const tag = (e.target && e.target.tagName) || '';
    if (/INPUT|TEXTAREA|SELECT/.test(tag) || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === 't' || e.key === 'T' || e.key === 'е' || e.key === 'Е') theatre = !theatre;
  }

  async function showConfirm(danger) {
    await ask({
      title: $t('g.card.title'),
      message: danger ? $t('g.dlg.messageDanger') : $t('g.dlg.message'),
      confirm: danger ? $t('g.danger') : $t('g.dlg.leave'),
      danger,
    });
  }
  const demoPeople = [
    { id: 'a1', name: 'Alice Moreau' }, { id: 'b2', name: 'Bob Ivanov' }, { id: 'c3', name: 'Carol Chen' },
    { id: 'd4', name: 'Дмитрий Новак' }, { id: 'e5', name: 'Егор' },
  ];
</script>

<svelte:window on:keydown={onKey} />

<div class="gal">
  <header class="top">
    <h1>{$t('g.title')}</h1>
    <div class="ctl">
      <span class="lbl">{$t('g.theme')}</span>
      <Tabs variant="segment" label={$t('g.theme')} value={$theme === 'light' ? 'light' : 'dark'}
        items={[{ id: 'dark', label: $t('g.theme.dark'), icon: 'moon' }, { id: 'light', label: $t('g.theme.light'), icon: 'sun' }]}
        on:change={(e) => theme.set(e.detail)} />
      <span class="lbl">{$t('g.density')}</span>
      <Tabs variant="segment" label={$t('g.density')} bind:value={density}
        items={[{ id: 'comfortable', label: $t('g.density.comfortable') }, { id: 'compact', label: $t('g.density.compact') }]} />
    </div>
  </header>

  <nav class="toc" aria-label={$t('g.nav')}>
    {#each sections as s}<a href="#g-{s}">{$t('g.s.' + s)}</a>{/each}
  </nav>

  <main>
    <!-- ================= tokens ================= -->
    <section id="g-tokens">
      <h2>{$t('g.s.tokens')}</h2>
      {#each Object.entries(swatches) as [grp, list]}
        <h3>{$t('g.t.' + grp)}</h3>
        <div class="sw-row">
          {#each list as v}
            <div class="sw"><span class="chip" style="background:var(--{v})"></span><code>--{v}</code></div>
          {/each}
        </div>
      {/each}

      <h3>{$t('g.t.type')}</h3>
      <div class="col">
        {#each typeScale as [k, w]}
          <div class="type-row"><code>{k}</code><span style="font-size:var(--fs-{k});line-height:var(--lh-{k});font-weight:var(--{w})">{$t('g.sample')}</span></div>
        {/each}
      </div>

      <h3>{$t('g.t.space')}</h3>
      <div class="row">
        {#each spaces as n}<div class="space"><span style="width:var(--sp-{n});height:var(--sp-{n})"></span><code>{n * 4}</code></div>{/each}
      </div>

      <h3>{$t('g.t.radii')}</h3>
      <div class="row">
        {#each radii as [k, px]}<div class="rad"><span style="border-radius:var(--r-{k})"></span><code>{k} {px}</code></div>{/each}
      </div>

      <h3>{$t('g.t.shadows')}</h3>
      <div class="row">
        {#each ['sm', 'md', 'lg', 'xl'] as k}<div class="shd" style="box-shadow:var(--shadow-{k})"><code>{k}</code></div>{/each}
      </div>

      <h3>{$t('g.t.motion')}</h3>
      <div class="row"><code>--dur-fast 120ms</code><code>--dur 160ms</code><code>--dur-slow 200ms</code><code>--ease</code></div>
    </section>

    <!-- ================= icons ================= -->
    <section id="g-icons">
      <h2>{$t('g.s.icons')}</h2>
      <div class="icons">
        {#each Object.keys(ICONS) as n}
          <div class="ic"><Icon name={n} size={20} /><code>{n}</code></div>
        {/each}
      </div>
      <div class="row">
        <Icon name="mic" size={16} /><Icon name="mic" size={20} /><Icon name="mic" size={24} />
        <Icon name="spinner" size={20} />
      </div>
    </section>

    <!-- ================= buttons ================= -->
    <section id="g-buttons">
      <h2>{$t('g.s.buttons')}</h2>
      <h3>{$t('g.states')}</h3>
      <div class="row">
        <Button variant="primary">{$t('g.primary')}</Button>
        <Button>{$t('g.secondary')}</Button>
        <Button variant="ghost">{$t('g.ghost')}</Button>
        <Button variant="danger" icon="trash">{$t('g.danger')}</Button>
        <Button variant="danger-solid">{$t('g.dangerSolid')}</Button>
      </div>
      <div class="row">
        <Button variant="primary" disabled>{$t('g.disabled')}</Button>
        <Button disabled>{$t('g.disabled')}</Button>
        <Button variant="ghost" disabled>{$t('g.disabled')}</Button>
        <Button variant="primary" loading>{$t('g.loading')}</Button>
        <Button loading>{$t('g.loading')}</Button>
        <Button pressed={true} icon="pin">{$t('g.pressed')}</Button>
      </div>
      <h3>{$t('g.sizes')}</h3>
      <div class="row">
        <Button variant="primary" size="sm" icon="plus">{$t('g.withIcon')}</Button>
        <Button variant="primary" icon="plus">{$t('g.withIcon')}</Button>
        <Button variant="primary" size="lg" icon="plus" iconRight="chevronRight">{$t('g.withIcon')}</Button>
      </div>
      <h3>IconButton</h3>
      <div class="row">
        <IconButton icon="mic" label={$t('g.mute')} />
        <IconButton icon="screenUp" label={$t('g.share')} variant="subtle" />
        <IconButton icon="video" label={$t('g.share')} pressed={true} />
        <IconButton icon="settings" label={$t('g.menu.open')} size="sm" />
        <IconButton icon="settings" label={$t('g.menu.open')} size="lg" />
        <IconButton icon="trash" label={$t('g.danger')} variant="danger" />
        <IconButton icon="hangup" label={$t('g.hangup')} variant="live" />
        <IconButton icon="mic" label={$t('g.mute')} disabled />
      </div>
    </section>

    <!-- ================= fields ================= -->
    <section id="g-fields">
      <h2>{$t('g.s.fields')}</h2>
      <div class="grid2">
        <Input label={$t('g.f.name')} hint={$t('g.f.hint')} bind:value={name} />
        <Input label={$t('g.f.name')} error={$t('g.f.error')} bind:value={nameBad} />
        <Input icon="search" placeholder={$t('g.f.search')} aria-label={$t('g.f.search')} value="" />
        <Input label={$t('g.f.name')} value="Alice" disabled />
        <Select label={$t('g.f.lang')} options={langOptions} bind:value={lang} />
        <Select label={$t('g.f.lang')} options={langOptions} value="en" disabled />
        <Textarea label={$t('g.f.about')} hint={$t('g.f.hint')} bind:value={about} />
        <Textarea label={$t('g.f.about')} error={$t('g.f.error')} value="…" />
      </div>
      <div class="grid2">
        <div class="col">
          <Toggle bind:checked={tgNotify} label={$t('g.f.notify')} hint={$t('g.f.notifyHint')} />
          <Toggle bind:checked={tgSound} label={$t('g.f.sound')} />
          <Toggle checked={true} label={$t('g.disabled')} disabled />
        </div>
        <div class="col">
          <Checkbox bind:checked={cbA} label={$t('g.f.remember')} />
          <Checkbox bind:checked={cbB} label={$t('g.f.all')} indeterminate={cbA && !cbB} />
          <Checkbox checked={true} label={$t('g.disabled')} disabled />
        </div>
      </div>
    </section>

    <!-- ================= tabs and badges ================= -->
    <section id="g-nav">
      <h2>{$t('g.s.nav')}</h2>
      <Tabs items={tabItems} bind:value={tab} label={$t('g.s.nav')} />
      <div class="row"><Tabs variant="segment" items={tabItems.slice(0, 3)} bind:value={tab} label={$t('g.s.nav')} /></div>
      <h3>Badge</h3>
      <div class="row">
        <Badge>{$t('g.badge.neutral')}</Badge>
        <Badge variant="accent">{$t('g.badge.accent')}</Badge>
        <Badge variant="ok" dot>{$t('g.badge.ok')}</Badge>
        <Badge variant="warn" icon="warning">{$t('g.badge.warn')}</Badge>
        <Badge variant="danger">{$t('g.badge.danger')}</Badge>
        <Badge variant="info">{$t('g.badge.info')}</Badge>
        <Badge size="sm">3</Badge>
      </div>
      <div class="row">
        <Badge kind="live" />
        <Badge kind="quality">1440p60</Badge>
        <Badge kind="quality">4K60</Badge>
        <Badge kind="direct" />
        <Badge kind="relay" />
        <Badge icon="lock">{$t('g.badge.e2e')}</Badge>
      </div>
    </section>

    <!-- ================= people ================= -->
    <section id="g-people">
      <h2>{$t('g.s.people')}</h2>
      <h3>{$t('g.p.members')}</h3>
      <div class="row">
        {#each [24, 32, 40, 56] as s, i}<Avatar id={demoPeople[i].id} name={demoPeople[i].name} size={s} />{/each}
        <Avatar id="x" name="Alice Moreau" src="data:image/svg+xml;utf8,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 40 40'%3E%3Crect width='40' height='40' fill='%23446'/%3E%3Ccircle cx='20' cy='16' r='7' fill='%23aab'/%3E%3Cpath d='M6 40c2-12 26-12 28 0z' fill='%23aab'/%3E%3C/svg%3E" size={40} />
      </div>
      <h3>{$t('g.p.status')}</h3>
      <div class="row">
        <Avatar id="a1" name="Alice Moreau" status="online" />
        <Avatar id="b2" name="Bob Ivanov" status="away" />
        <Avatar id="c3" name="Carol Chen" status="busy" />
        <Avatar id="d4" name="Дмитрий Новак" status="offline" />
        <Avatar id="e5" name="Егор" speaking={true} />
      </div>
      <h3>{$t('g.p.list')}</h3>
      <div class="list">
        <ListItem title="Alice Moreau" subtitle={$t('g.p.hello')} selected>
          <Avatar slot="lead" id="a1" name="Alice Moreau" size={36} status="online" />
          <span slot="meta">{$t('g.p.now')}</span>
        </ListItem>
        <ListItem title="Night shift" subtitle={$t('g.p.unread')} unread>
          <Avatar slot="lead" id="g9" name="Night shift" size={36} />
          <svelte:fragment slot="meta"><span>{$t('g.p.now')}</span><Badge variant="accent" size="sm">4</Badge></svelte:fragment>
        </ListItem>
        <ListItem title="Bob Ivanov" subtitle={$t('g.p.hello')}>
          <Avatar slot="lead" id="b2" name="Bob Ivanov" size={36} />
          <IconButton slot="trail" icon="more" label={$t('ui.more')} size="sm" />
        </ListItem>
        <ListItem title="Carol Chen" disabled>
          <Avatar slot="lead" id="c3" name="Carol Chen" size={36} />
        </ListItem>
      </div>
    </section>

    <!-- ================= overlays ================= -->
    <section id="g-overlay">
      <h2>{$t('g.s.overlay')}</h2>
      <div class="row">
        <Menu items={menuItems} on:select={(e) => (picked = e.detail)} label={$t('g.menu.open')}>
          <Button slot="trigger" let:toggle let:open on:click={toggle} icon="more" aria-haspopup="menu" aria-expanded={open}>{$t('g.menu.open')}</Button>
        </Menu>
        {#if picked}<span class="muted">{$t('g.menu.picked', { id: picked })}</span>{/if}
        <Tooltip text={$t('g.tip.text')}><Button variant="ghost">{$t('g.tip.hover')}</Button></Tooltip>
      </div>
      <div class="row">
        <Button on:click={() => showConfirm(false)}>{$t('g.dlg.confirm')}</Button>
        <Button variant="danger" on:click={() => showConfirm(true)}>{$t('g.dlg.danger')}</Button>
        <Button on:click={() => (modalOpen = true)}>{$t('g.dlg.modal')}</Button>
      </div>
      <div class="row">
        <Button on:click={() => toast({ kind: 'info', message: $t('g.toast.info') })}>{$t('g.toast.show')}: info</Button>
        <Button on:click={() => toast({ kind: 'success', message: $t('g.toast.success') })}>success</Button>
        <Button on:click={() => toast({ kind: 'warning', message: $t('g.toast.warning') })}>warning</Button>
        <Button on:click={() => toast({ kind: 'error', message: $t('g.toast.error'), action: { label: $t('g.toast.action'), run: () => {} } })}>error</Button>
      </div>
      {#if modalOpen}
      <Modal label={$t('g.dlg.modalTitle')} width={440} on:close={() => (modalOpen = false)}>
        <div class="modal-body">
          <h3>{$t('g.dlg.modalTitle')}</h3>
          <p class="muted">{$t('g.dlg.modalBody')}</p>
          <div class="row end"><Button on:click={() => (modalOpen = false)}>{$t('common.close')}</Button><Button variant="primary" on:click={() => (modalOpen = false)}>{$t('common.save')}</Button></div>
        </div>
      </Modal>
      {/if}
    </section>

    <!-- ================= surfaces ================= -->
    <section id="g-surfaces">
      <h2>{$t('g.s.surfaces')}</h2>
      <div class="grid3">
        <Card variant="raised"><h3>{$t('g.card.title')}</h3><p class="muted">{$t('g.card.text')}</p>
          <svelte:fragment slot="footer"><Button size="sm">{$t('common.cancel')}</Button><Button size="sm" variant="primary">{$t('common.save')}</Button></svelte:fragment></Card>
        <Card variant="flat"><h3>{$t('g.card.flat')}</h3><p class="muted">{$t('g.card.text')}</p></Card>
        <Card variant="outlined"><h3>{$t('g.card.outlined')}</h3><p class="muted">{$t('g.card.text')}</p></Card>
        <Card interactive><h3>{$t('g.card.interactive')}</h3><p class="muted">{$t('g.card.text')}</p></Card>
        <Card interactive selected><h3>{$t('g.card.selected')}</h3><p class="muted">{$t('g.card.text')}</p></Card>
      </div>
      <Divider />
      <Divider>{$t('g.divider.label')}</Divider>
      <h3>{$t('g.skeleton')}</h3>
      <div class="row top">
        <Skeleton circle height={40} />
        <div class="grow"><Skeleton lines={3} height={12} /></div>
      </div>
      <h3>ScrollArea</h3>
      <div class="scroll-box">
        <ScrollArea label={$t('g.scroll')}>
          <div class="pad">
            {#each Array(14) as _, i}<p class="muted">{$t('g.scroll')} · {i + 1}</p>{/each}
          </div>
        </ScrollArea>
      </div>
    </section>

    <!-- ================= message feed ================= -->
    <section id="g-feed">
      <h2>{$t('g.s.feed')}</h2>
      <div class="row">
        <span class="lbl">{$t('g.feed.pref')}</span>
        <Tabs variant="segment" items={feedPrefItems} bind:value={feedPrefLocal} label={$t('g.feed.pref')} />
        <div class="w160"><Input type="number" label={$t('g.feed.members')} bind:value={feedMembers} min="1" max="500" /></div>
      </div>
      <p class="muted small">{$t('g.feed.shown', { mode: $t('g.feed.' + shownMode), n: COMPACT_ABOVE })}</p>
      <div class="feed" data-feed={shownMode}>
        <div class="msg"><Avatar id="a1" name="Alice Moreau" size={shownMode === 'compact' ? 28 : 36} />
          <div class="body"><div class="nm"><b>Alice Moreau</b><span>{$t('g.p.now')}</span></div><div class="tx">{$t('g.feed.m1')}</div></div></div>
        <div class="msg own"><Avatar id="me" name="Me" size={shownMode === 'compact' ? 28 : 36} />
          <div class="body"><div class="nm"><b>Bob Ivanov</b><span>{$t('g.p.now')}</span></div><div class="tx">{$t('g.feed.m2')}</div></div></div>
        <div class="msg"><Avatar id="c3" name="Carol Chen" size={shownMode === 'compact' ? 28 : 36} />
          <div class="body"><div class="nm"><b>Carol Chen</b><span>{$t('g.p.now')}</span></div><div class="tx">{$t('g.feed.m3')}</div></div></div>
        <div class="msg"><Avatar id="d4" name="Дмитрий Новак" size={shownMode === 'compact' ? 28 : 36} />
          <div class="body"><div class="nm"><b>Дмитрий Новак</b><span>{$t('g.p.now')}</span></div><div class="tx">{$t('g.feed.m4')}</div></div></div>
      </div>
    </section>

    <!-- ================= room layout ================= -->
    <section id="g-room">
      <h2>{$t('g.s.room')}</h2>
      <div class="row">
        <Toggle bind:checked={roomSide} label={$t('g.room.side')} disabled={theatre} />
        <Toggle bind:checked={roomStrip} label={$t("g.room.strip")} />
        <Toggle bind:checked={roomChat} label={$t('g.room.chat')} disabled={theatre} />
        <Button pressed={theatre} icon="theater" on:click={() => (theatre = !theatre)}>{$t('g.room.theatreKey')}</Button>
        <strong class="num">{$t('g.room.width', { w: videoW })}</strong>
      </div>
      <div class="room-wrap">
        <div class="room" style="width:{WIN}px">
          {#if sideW}<aside class="r-side" style="width:{sideW}px"><span class="muted">{$t('g.room.side')}</span></aside>{/if}
          <div class="r-video" style="width:{videoW}px">
            <div class="vid">
              <div class="vtop"><Badge kind="live" /><Badge kind="quality">{$t('g.room.q')}</Badge><Badge kind="direct" /><span class="vstat">{$t('g.room.status')}</span></div>
              <span class="vlabel">{$t('g.room.stream')} · {videoW} px</span>
            </div>
          </div>
          <div class="r-part" style="width:{partW}px">
            <IconButton icon={roomStrip ? 'chevronLeft' : 'chevronRight'} label={roomStrip ? $t('g.room.expand') : $t('g.room.collapse')} size="sm" on:click={() => (roomStrip = !roomStrip)} />
            {#each demoPeople as p, i}
              {#if roomStrip}<Avatar id={p.id} name={p.name} size={36} speaking={i === 1} />
              {:else}<div class="ptile"><Avatar id={p.id} name={p.name} size={36} speaking={i === 1} /><span>{p.name}</span></div>{/if}
            {/each}
          </div>
          {#if chatW}<aside class="r-chat" style="width:{chatW}px"><span class="muted">{$t('g.room.chat')}</span></aside>{/if}
        </div>
      </div>
    </section>
  </main>
</div>

<Dialog />
<Toast />

<style>
  .gal { height: 100%; overflow-y: auto; background: var(--bg); color: var(--text); scroll-behavior: smooth; }
  .top {
    position: sticky; top: 0; z-index: var(--z-sticky); display: flex; align-items: center; justify-content: space-between; gap: var(--sp-4); flex-wrap: wrap;
    padding: var(--sp-3) var(--sp-6); background: var(--surface-1); border-bottom: 1px solid var(--border);
  }
  .ctl { display: flex; align-items: center; gap: var(--sp-3); flex-wrap: wrap; }
  .lbl { font-size: var(--fs-sm); color: var(--muted); }
  .toc { display: flex; gap: var(--sp-1); flex-wrap: wrap; padding: var(--sp-3) var(--sp-6) 0; }
  .toc a { padding: 4px 10px; border-radius: var(--r-pill); background: var(--surface-2); color: var(--muted); font-size: var(--fs-sm); text-decoration: none; }
  .toc a:hover { color: var(--text); text-decoration: none; background: var(--surface-3); }
  main { padding: var(--sp-4) var(--sp-6) var(--sp-12); max-width: 1480px; }
  section { padding: var(--sp-6) 0; border-bottom: 1px solid var(--border); display: flex; flex-direction: column; gap: var(--sp-3); }
  h2 { font-size: var(--fs-xl); line-height: var(--lh-xl); }
  h3 { font-size: var(--fs-sm); line-height: var(--lh-sm); font-weight: var(--fw-semibold); color: var(--muted); margin-top: var(--sp-2); }
  code { font: 11.5px/1.4 var(--mono); color: var(--muted); }
  .row { display: flex; align-items: center; gap: var(--sp-3); flex-wrap: wrap; }
  .row.top { align-items: flex-start; max-width: 420px; }
  .row.end { justify-content: flex-end; }
  .col { display: flex; flex-direction: column; gap: var(--sp-3); }
  .grow { flex: 1; min-width: 0; }
  .grid2 { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: var(--sp-4); max-width: 900px; }
  .grid3 { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: var(--sp-4); }
  .muted { color: var(--muted); }
  .small { font-size: var(--fs-sm); }
  .w160 { width: 160px; }

  .sw-row { display: flex; flex-wrap: wrap; gap: var(--sp-3); }
  .sw { display: flex; flex-direction: column; gap: 4px; width: 104px; }
  .sw .chip { height: 40px; border-radius: var(--r-md); border: 1px solid var(--border-strong); }
  .type-row { display: flex; align-items: baseline; gap: var(--sp-4); }
  .type-row code { width: 48px; }
  .space { display: flex; flex-direction: column; align-items: center; gap: 4px; }
  .space span { display: block; background: var(--accent); border-radius: 2px; }
  .rad { display: flex; flex-direction: column; align-items: center; gap: 4px; }
  .rad span { width: 56px; height: 56px; background: var(--surface-3); border: 1px solid var(--border-strong); }
  .shd { width: 88px; height: 56px; border-radius: var(--r-lg); background: var(--surface-1); display: grid; place-items: center; }
  .icons { display: grid; grid-template-columns: repeat(auto-fill, minmax(96px, 1fr)); gap: var(--sp-2); }
  .ic { display: flex; flex-direction: column; align-items: center; gap: 6px; padding: var(--sp-3) 4px; border-radius: var(--r-lg); background: var(--surface-2); }
  .list { max-width: 420px; display: flex; flex-direction: column; gap: 2px; }
  .modal-body { padding: var(--sp-5); display: flex; flex-direction: column; gap: var(--sp-3); }
  .pad { padding: var(--sp-3); }
  .pad p { margin: 0 0 var(--sp-2); }
  .scroll-box { height: 140px; max-width: 420px; background: var(--surface-2); border-radius: var(--r-xl); display: flex; flex-direction: column; }

  /* message feed: the same markup in two looks */
  .feed { max-width: 640px; display: flex; flex-direction: column; padding: var(--sp-3); border-radius: var(--r-xl); background: var(--stage); border: 1px solid var(--border); }
  .msg { display: flex; gap: 10px; align-items: flex-end; }
  .nm { display: flex; gap: 8px; align-items: baseline; }
  .nm b { font-size: var(--fs-sm); font-weight: var(--fw-semibold); }
  .nm span { font-size: var(--fs-2xs); color: var(--faint); }
  .tx { font-size: var(--fs-chat); line-height: var(--lh-chat); overflow-wrap: anywhere; }
  .feed[data-feed="bubbles"] { gap: 6px; }
  .feed[data-feed="bubbles"] .body { background: var(--bubble-in); padding: 7px 13px 8px; border-radius: var(--r-bubble) var(--r-bubble) var(--r-bubble) 6px; max-width: 70%; }
  .feed[data-feed="bubbles"] .msg.own { flex-direction: row-reverse; }
  .feed[data-feed="bubbles"] .msg.own :global(.relative) { display: none; }
  .feed[data-feed="bubbles"] .msg.own .body { background: var(--bubble-out); color: var(--bubble-out-text); border-radius: var(--r-bubble) var(--r-bubble) 6px var(--r-bubble); }
  .feed[data-feed="bubbles"] .msg.own .nm { display: none; }
  .feed[data-feed="compact"] { gap: 0; }
  .feed[data-feed="compact"] .msg { align-items: flex-start; padding: 4px 6px; border-radius: var(--r-md); }
  .feed[data-feed="compact"] .msg:hover { background: var(--hover); }
  .feed[data-feed="compact"] .tx { font-size: var(--fs-md); line-height: var(--lh-md); }

  /* room layout: a 1440 px window drawn at 60% */
  .room-wrap { overflow: hidden; width: calc(1440px * 0.6); height: calc(560px * 0.6); border: 1px solid var(--border-strong); border-radius: var(--r-xl); max-width: 100%; }
  .room { zoom: 0.6; height: 560px; display: flex; background: var(--stage); }
  .r-side { flex: none; background: var(--surface-1); border-right: 1px solid var(--border); padding: var(--sp-4); }
  .r-chat { flex: none; background: var(--surface-1); border-left: 1px solid var(--border); padding: var(--sp-4); }
  .r-video { flex: none; padding: var(--sp-3); transition: width var(--dur-slow) var(--ease); }
  .vid { position: relative; height: 100%; border-radius: var(--r-xl); background: linear-gradient(135deg, var(--tile), var(--surface-3)); display: grid; place-items: center; }
  .vtop { position: absolute; top: var(--sp-3); left: var(--sp-3); display: flex; gap: 6px; align-items: center; }
  .vstat { font-size: var(--fs-xs); color: var(--muted); margin-left: 6px; }
  .vlabel { font-size: var(--fs-lg); color: var(--faint); font-variant-numeric: tabular-nums; }
  .r-part { flex: none; display: flex; flex-direction: column; align-items: center; gap: var(--sp-2); padding: var(--sp-3) 0; transition: width var(--dur-slow) var(--ease); }
  .ptile { display: flex; align-items: center; gap: 8px; width: calc(100% - 16px); padding: 6px; border-radius: var(--r-lg); background: var(--tile); font-size: var(--fs-xs); }
  .ptile span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .num { font-variant-numeric: tabular-nums; }
</style>
