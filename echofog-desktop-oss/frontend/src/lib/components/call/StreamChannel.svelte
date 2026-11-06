<script>
    /**
     * A stream channel, as a model on fake data (opened with ?mock&streamchannel; there is no core
     * behind it, so it is not part of a real build). The viewer sees the video big, the channel's
     * chat by role and the number of viewers. Only roles with the right talk and show. The owner and
     * moderators moderate the chat (delete, ban, slow mode).
     *
     * data: { title, host, viewers, roles: { id: { label, tone } }, people: [{ id, name, hue, role, onStage }], messages: [...] }
     */
    import { onMount, onDestroy } from 'svelte';
    import Icon from '../ui/Icon.svelte';
    import IconButton from '../ui/IconButton.svelte';
    import Badge from '../ui/Badge.svelte';
    import Avatar from '../ui/Avatar.svelte';
    import Button from '../ui/Button.svelte';
    import CameraPip from './CameraPip.svelte';
    import VideoView from './VideoView.svelte';
    import { autohide } from './autohide.js';
    import './call-icons.js';
    import './call-ui.css';
    import { t } from '../../i18n/index.js';

    export let data;
    export let onClose = () => {};

    // who the viewer is in this channel: a role of the fake roster
    let me = 'viewer'; // viewer | booster | moderator | owner
    let chatPos = 'right'; // right | bottom | hidden ("theatre")
    let cam = 'overlay'; // overlay | tile
    let camState = { x: 1, y: 1, w: 0.2, shape: 'rounded', hidden: false };
    let slow = 0; // seconds between messages; 0 is off
    let draft = '';
    let cooldown = 0;
    let viewers = data.viewers;
    let elapsed = 2 * 3600 + 14 * 60 + 8; // the stream's running time, seconds
    let unread = 0;
    let messages = data.messages.map((m, i) => ({ id: i, ...m }));
    let banned = new Set();
    let nextId = messages.length;
    let sceneCanvas;
    let camMedia = null;
    let videoBox;
    let list;

    $: canModerate = me === 'owner' || me === 'moderator';
    $: canStage = me === 'owner' || me === 'moderator';
    $: iAmBanned = banned.has('me');
    $: stage = data.people.filter((p) => p.onStage);
    $: if (chatPos !== 'hidden') unread = 0;

    const roleOf = (id) => data.roles[id];
    const hue = (n) => `hsl(${n} 45% 45%)`;

    // --- the picture: drawn at 30 fps; setInterval so that it also runs in a hidden window
    let timer;
    let camTimer;
    function drawScene(c, camera) {
        const g = c.getContext('2d');
        let k = 0;
        return setInterval(() => {
            k += 1;
            const { width: w, height: h } = c;
            if (camera) {
                const gr = g.createLinearGradient(0, 0, w, h);
                gr.addColorStop(0, '#3b2f63');
                gr.addColorStop(1, '#0f3443');
                g.fillStyle = gr;
                g.fillRect(0, 0, w, h);
                g.fillStyle = '#f2c9a0';
                g.beginPath();
                g.arc(w / 2 + Math.sin(k / 50) * 8, h / 2 + 6, h * 0.24, 0, Math.PI * 2);
                g.fill();
                g.fillStyle = '#2b2d31';
                g.fillRect(w / 2 - h * 0.36, h * 0.8, h * 0.72, h * 0.3);
                return;
            }
            const sky = g.createLinearGradient(0, 0, 0, h);
            sky.addColorStop(0, '#150d2b');
            sky.addColorStop(0.55, '#4a2150');
            sky.addColorStop(1, '#e8955a');
            g.fillStyle = sky;
            g.fillRect(0, 0, w, h);
            g.fillStyle = '#ffe4bd';
            g.beginPath();
            g.arc(w * 0.74, h * 0.27, h * 0.07, 0, Math.PI * 2);
            g.fill();
            g.fillStyle = '#3a1d44';
            g.beginPath();
            g.moveTo(0, h * 0.7);
            for (let i = 0; i <= 10; i++) g.lineTo((w * i) / 10, h * (0.5 + 0.08 * Math.sin(i * 1.7)));
            g.lineTo(w, h);
            g.lineTo(0, h);
            g.fill();
            g.fillStyle = '#0d0716';
            const bx = w * 0.55 + Math.sin(k / 40) * w * 0.03;
            g.fillRect(bx, h * 0.34, w * 0.14, h * 0.5);
            g.fillStyle = '#ff7a3c';
            g.beginPath();
            g.arc(bx + w * 0.03, h * 0.4, 9, 0, Math.PI * 2);
            g.arc(bx + w * 0.11, h * 0.4, 9, 0, Math.PI * 2);
            g.fill();
            g.fillStyle = 'rgba(0,0,0,.55)';
            g.fillRect(30, 30, w * 0.22, 12);
            g.fillStyle = '#c8382f';
            g.fillRect(30, 30, w * 0.22 * (0.4 + 0.3 * Math.abs(Math.sin(k / 90))), 12);
            g.fillStyle = '#e8d9b0';
            g.font = '600 26px Inter, sans-serif';
            g.textAlign = 'center';
            g.fillText('• • •', w / 2, h * 0.92);
        }, 33);
    }

    // chat that keeps coming while the stream runs
    const LINES = ['gg', 'that dodge was clean', 'which preset is this?', 'phase 2 is the hard one', 'first time here, hello!', 'how many attempts so far?'];
    let chatTimer;
    let viewTimer;
    let clockTimer;
    onMount(() => {
        timer = drawScene(sceneCanvas, false);
        const cc = document.createElement('canvas');
        cc.width = 480;
        cc.height = 360;
        camTimer = drawScene(cc, true);
        camMedia = cc.captureStream(30);
        chatTimer = setInterval(() => {
            const p = data.people.filter((x) => x.id !== 'alice' && !banned.has(x.id))[Math.floor(Math.random() * 4)] || data.people[2];
            push({ from: p.id, text: LINES[Math.floor(Math.random() * LINES.length)] });
        }, 4500);
        clockTimer = setInterval(() => (elapsed += 1), 1000);
        viewTimer = setInterval(() => (viewers = Math.max(1, viewers + Math.round((Math.random() - 0.45) * 9))), 3000);
    });
    onDestroy(() => {
        clearInterval(timer);
        clearInterval(camTimer);
        clearInterval(chatTimer);
        clearInterval(viewTimer);
        clearInterval(clockTimer);
    });

    function push(m) {
        if (banned.has(m.from)) return;
        messages = [...messages.slice(-199), { id: nextId++, at: Date.now(), ...m }];
        if (chatPos === 'hidden') unread += 1;
        else queueMicrotask(() => list && (list.scrollTop = list.scrollHeight));
    }

    function send() {
        const text = draft.trim();
        if (!text || cooldown > 0 || iAmBanned) return;
        push({ from: 'me', text });
        draft = '';
        if (slow && !canModerate) {
            cooldown = slow;
            const iv = setInterval(() => {
                cooldown -= 1;
                if (cooldown <= 0) clearInterval(iv);
            }, 1000);
        }
    }

    const removeMsg = (id) => (messages = messages.map((m) => (m.id === id ? { id, removed: true } : m)));
    function ban(id) {
        banned = new Set([...banned, id]);
        messages = messages.map((m) => (m.from === id && !m.removed ? { id: m.id, removed: true } : m));
        messages = [...messages, { id: nextId++, system: 'banned', who: id }];
    }

    function fullscreen() {
        if (document.fullscreenElement) document.exitFullscreen();
        else videoBox?.requestFullscreen?.().catch(() => {});
    }

    function onKey(e) {
        if (e.ctrlKey || e.metaKey || e.altKey) return;
        if (e.target?.closest?.('input, textarea, select')) return;
        const k = e.key.toLowerCase();
        if (e.key === 'Escape') onClose();
        else if (k === 'f' || k === 'а') {
            e.preventDefault();
            fullscreen();
        } else if (k === 't' || k === 'е') {
            e.preventDefault();
            chatPos = chatPos === 'hidden' ? 'right' : 'hidden';
        }
    }

    const clock = (sec) => [Math.floor(sec / 3600), Math.floor(sec / 60) % 60, sec % 60].map((n) => String(n).padStart(2, '0')).join(':');
    const time = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    const nameOf = (id) => (id === 'me' ? $t('call.ch.you') : data.people.find((p) => p.id === id)?.name || id);
    const roleIdOf = (id) => (id === 'me' ? me : data.people.find((p) => p.id === id)?.role);
</script>

<svelte:window on:keydown={onKey} />

<div class="ch" role="region" aria-label={$t('call.ch.title')}>
    <header class="top">
        <div class="ttl">
            <Icon name="live" size={17} />
            <b class="ell">{data.channel || data.title}</b>
            <span class="st ell">
                {data.space || ''}<span class="sep">·</span><span class="enc"><Icon name="lock" size={13} />{$t('call.e2e')}</span><span class="sep">·</span>{$t('callpath.badgeDirect')}
            </span>
        </div>
        <div class="tools">
            <Badge kind="live" size="sm" />
            <Badge kind="quality" size="sm">1440p60</Badge>
            <IconButton size="sm" icon="close" label="{$t('call.ch.close')} (Esc)" on:click={onClose} />
        </div>
    </header>

    <div class="bar2">
        <div class="grp"><span class="lab">{$t('call.ch.camLabel')}</span>
            <div class="seg" role="group" aria-label={$t('call.ch.camera')}>
                <button class:on={cam === 'overlay'} aria-pressed={cam === 'overlay'} title={$t('call.ch.camOverlay')} on:click={() => (cam = 'overlay')}>{$t('call.ch.camOver')}</button>
                <button class:on={cam === 'tile'} aria-pressed={cam === 'tile'} title={$t('call.ch.camTile')} on:click={() => (cam = 'tile')}>{$t('call.ch.camTileShort')}</button>
            </div>
        </div>
        <div class="grp"><span class="lab">{$t('call.ch.chatLabel')}</span>
            <div class="seg" role="group" aria-label={$t('call.ch.chat')}>
                <button class:on={chatPos === 'right'} aria-pressed={chatPos === 'right'} title={$t('call.ch.chatRight')} on:click={() => (chatPos = 'right')}>{$t('call.ch.posRight')}</button>
                <button class:on={chatPos === 'bottom'} aria-pressed={chatPos === 'bottom'} title={$t('call.ch.chatBottom')} on:click={() => (chatPos = 'bottom')}>{$t('call.ch.posBottom')}</button>
                <button class:on={chatPos === 'hidden'} aria-pressed={chatPos === 'hidden'} title="{$t('call.ch.chatHidden')} (T)" on:click={() => (chatPos = chatPos === 'hidden' ? 'right' : 'hidden')}>{$t('call.ch.posHidden')}</button>
            </div>
        </div>
        <span class="grow"></span>
        <label class="as">
            <span>{$t('call.ch.viewAs')}</span>
            <select bind:value={me} aria-label={$t('call.ch.viewAs')}>
                <option value="viewer">{$t('call.ch.role.viewer')}</option>
                <option value="booster">{$t('call.ch.role.booster')}</option>
                <option value="moderator">{$t('call.ch.role.moderator')}</option>
                <option value="owner">{$t('call.ch.role.owner')}</option>
            </select>
        </label>
    </div>

    <div class="main" data-chat={chatPos}>
        <div class="viewer">
            <div class="vidwrap">
                <div class="vframe speak-ring" bind:this={videoBox} use:autohide data-main-frame>
                    <canvas bind:this={sceneCanvas} width="1600" height="900" class="scene" aria-label={$t('call.ch.placeholder')}></canvas>
                    <div class="absolute left-2 top-2 flex items-center gap-1">
                        <span class="vtag"><Icon name="eye" size={12} /> {viewers.toLocaleString()}</span>
                        <span class="vtag">{clock(elapsed)}</span>
                    </div>
                    {#if cam === 'overlay' && camMedia}
                        <CameraPip media={camMedia} state={camState} onChange={(patch) => (camState = { ...camState, ...patch })} />
                    {/if}
                    {#if cam === 'overlay' && camState.hidden}
                        <button class="vbtn ov absolute right-2 top-2" style="right:84px" title={$t('call.pip.show')} aria-label={$t('call.pip.show')} on:click={() => (camState = { ...camState, hidden: false })}><Icon name="video" size={16} /></button>
                    {/if}
                    <div class="ov absolute right-2 top-2 flex items-center gap-1">
                        <button class="vbtn" title={$t('call.streamSoundOff')} aria-label={$t('call.streamSoundOff')}><Icon name="volume" size={16} /></button>
                        <button class="vbtn" title="{$t('call.fullscreen')} (F)" aria-label={$t('call.fullscreen')} on:click={fullscreen}><Icon name="fullscreen" size={16} /></button>
                    </div>
                    <div class="ov absolute left-2 bottom-2 vtag" style="height:24px;font-size:var(--fs-xs)">{data.host}</div>
                </div>
            </div>

            <div class="info">
                <div class="who">
                    <Avatar id="alice" name={data.host} size={40} />
                    <div class="min-w-0">
                        <div class="name ell">{data.title}</div>
                        <div class="sub ell">{data.host} · {$t('call.ch.onStage', { n: stage.length })}</div>
                    </div>
                </div>
                <div class="pills">
                    <span class="pill tabular-nums" title={$t('call.ch.viewers')}><Icon name="eye" size={14} /> {$t('call.ch.viewersN', { n: viewers.toLocaleString() })}</span>
                    <span class="pill tabular-nums"><Icon name="clock" size={14} /> {clock(elapsed)}</span>
                    <span class="pill"><Icon name="shield" size={14} /> {$t('call.ch.relays', { n: 2 })}</span>
                </div>
                <div class="stage-row">
                    {#each stage as p (p.id)}
                        <span class="chip">
                            <Avatar id={p.id} name={p.name} size={20} />
                            {p.name}
                            <Badge size="sm" variant={roleOf(p.role).tone}>{$t('call.ch.role.' + p.role)}</Badge>
                        </span>
                    {/each}
                    {#if cam === 'tile' && camMedia}
                        <span class="camtile"><VideoView stream={camMedia} fit="cover" /></span>
                    {/if}
                    {#if canStage}
                        <Button size="sm" variant="secondary" icon="mic">{$t('call.ch.invite')}</Button>
                    {:else}
                        <span class="rule" title={$t('call.ch.rule')}><Icon name="lock" size={12} /> {$t('call.ch.rule')}</span>
                    {/if}
                </div>
            </div>
        </div>

        {#if chatPos !== 'hidden'}
            <aside class="chat" aria-label={$t('call.ch.chat')}>
                <div class="chat-head">
                    <b>{$t('call.ch.chat')}</b>
                    {#if slow}<Badge size="sm" variant="warn" icon="clock">{$t('call.ch.slowOn', { s: slow })}</Badge>{/if}
                    {#if me === 'owner'}
                        <label class="slow">
                            <span>{$t('call.ch.slow')}</span>
                            <select bind:value={slow} aria-label={$t('call.ch.slow')}>
                                <option value={0}>{$t('call.ch.slowOff')}</option>
                                <option value={5}>5 {$t('call.ch.sec')}</option>
                                <option value={15}>15 {$t('call.ch.sec')}</option>
                                <option value={60}>60 {$t('call.ch.sec')}</option>
                            </select>
                        </label>
                    {/if}
                </div>
                <div class="scroll" bind:this={list}>
                    {#each messages as m (m.id)}
                        {#if m.system}
                            <div class="cm sys"><Icon name="shield" size={13} /> {$t('call.ch.banned', { name: nameOf(m.who) })}</div>
                        {:else if m.removed}
                            <div class="cm sys"><Icon name="trash" size={13} /> {$t('call.ch.removed')}</div>
                        {:else}
                            {@const rid = roleIdOf(m.from)}
                            <div class="cm">
                                <Avatar id={m.from} name={nameOf(m.from)} size={26} />
                                <div class="body">
                                    <div class="nm">
                                        <b>{nameOf(m.from)}</b>
                                        {#if rid && rid !== 'viewer'}<Badge size="sm" variant={roleOf(rid).tone}>{$t('call.ch.role.' + rid)}</Badge>{/if}
                                        {#if m.at}<time>{time(m.at)}</time>{/if}
                                    </div>
                                    <div class="tx">{m.text}</div>
                                </div>
                                {#if canModerate && m.from !== 'me' && rid !== 'owner'}
                                    <span class="acts">
                                        <button class="vb" title={$t('call.ch.delete')} aria-label={$t('call.ch.delete')} on:click={() => removeMsg(m.id)}><Icon name="trash" size={14} /></button>
                                        {#if me === 'owner'}
                                            <button class="vb" title={$t('call.ch.ban')} aria-label={$t('call.ch.ban')} on:click={() => ban(m.from)}><Icon name="ban" size={14} /></button>
                                        {/if}
                                    </span>
                                {/if}
                            </div>
                        {/if}
                    {/each}
                </div>
                <form class="compose" on:submit|preventDefault={send}>
                    <input
                        bind:value={draft}
                        disabled={iAmBanned}
                        placeholder={iAmBanned ? $t('call.ch.youBanned') : cooldown > 0 ? $t('call.ch.wait', { s: cooldown }) : $t('call.ch.write')}
                        aria-label={$t('call.ch.write')}
                    />
                    <IconButton size="sm" type="submit" icon="send" label={$t('call.ch.send')} disabled={!draft.trim() || cooldown > 0 || iAmBanned} />
                </form>
                {#if me !== 'viewer'}
                    <div class="legend">{$t('call.ch.you')}: {$t('call.ch.role.' + me)}</div>
                {/if}
            </aside>
        {:else if unread}
            <button class="unread" on:click={() => (chatPos = 'right')}>{$t('call.ch.unread', { n: unread })}</button>
        {/if}
    </div>
</div>

<style>
    .ch { position: absolute; inset: 0; display: flex; flex-direction: column; background: var(--stage); color: var(--text); font-size: var(--fs-md); }
    .top { height: var(--top-h, 56px); flex: none; display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 0 16px; border-bottom: 1px solid var(--border); }
    .ttl b { font-size: var(--fs-lg); font-weight: var(--fw-bold); }
    .st { color: var(--muted); font-size: var(--fs-sm); display: inline-flex; align-items: center; gap: 6px; }
    .st .enc { display: inline-flex; align-items: center; gap: 4px; margin-left: 2px; }
    .bar2 { flex: none; display: flex; align-items: center; gap: 18px; padding: 10px 16px 0; }
    .grp { display: inline-flex; align-items: center; gap: 8px; }
    .lab { font-size: var(--fs-sm); color: var(--muted); }
    .bar2 .seg button { height: 28px; padding: 0 13px; border: none; border-radius: var(--r-pill); background: transparent; color: var(--muted); font-size: var(--fs-sm); font-weight: var(--fw-semibold); }
    .bar2 .seg button:hover:not(:disabled) { background: var(--hover); border: none; color: var(--text); }
    .bar2 .seg button.on, .bar2 .seg button.on:hover:not(:disabled) { background: var(--surface-3); color: var(--text); }
    .pills { display: inline-flex; gap: 8px; flex-wrap: wrap; }
    .pill { display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 12px; border-radius: var(--r-pill); background: var(--surface-2); color: var(--text); font-size: var(--fs-sm); font-weight: var(--fw-semibold); }
    .ttl { display: flex; align-items: center; gap: 8px; min-width: 0; }
    .ttl b { font-weight: var(--fw-semibold); }
    .ell { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .viewers { display: inline-flex; align-items: center; gap: 4px; color: var(--muted); font-size: var(--fs-sm); }
    .tools { display: flex; align-items: center; gap: 8px; flex: none; }
    .seg { display: flex; gap: 2px; padding: 2px; border-radius: var(--r-pill); background: var(--surface-2); }
    .as, .slow { white-space: nowrap; display: inline-flex; align-items: center; gap: 6px; font-size: var(--fs-xs); color: var(--muted); }
    select { height: var(--ctl-sm); padding: 0 8px; font-size: var(--fs-xs); border-radius: var(--r-md); background: var(--surface-2); border: 1px solid var(--border-strong); color: var(--text); }

    .main { flex: 1; min-height: 0; display: grid; grid-template-columns: minmax(0, 1fr) 320px; gap: 0; position: relative; }
    .main[data-chat='bottom'] { grid-template-columns: minmax(0, 1fr); grid-template-rows: minmax(0, 1fr) 240px; }
    .main[data-chat='hidden'] { grid-template-columns: minmax(0, 1fr); position: relative; }
    .viewer { min-width: 0; min-height: 0; display: flex; flex-direction: column; padding: 12px 16px 16px; gap: 12px; }
    .vidwrap { flex: 1; min-height: 0; display: flex; align-items: center; justify-content: center; }
    .vidwrap { container-type: size; }
    .vidwrap :global(.vframe) { width: min(100cqw, 100cqh * 16 / 9); height: min(100cqh, 100cqw * 9 / 16); }
    .scene { width: 100%; height: 100%; display: block; }
    .info { flex: none; display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
    .who { flex: 1 1 220px; display: flex; align-items: center; gap: 10px; min-width: 0; }
    .name { font-weight: var(--fw-semibold); }
    .sub { font-size: var(--fs-xs); color: var(--muted); }
    .stage-row { flex-basis: 100%; display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
    .chip { display: inline-flex; align-items: center; gap: 6px; height: 28px; padding: 0 8px 0 4px; border-radius: var(--r-md); background: var(--surface-2); font-size: var(--fs-xs); }
    .camtile { width: 48px; height: 36px; border-radius: var(--r-md); overflow: hidden; background: #000; }
    .rule { display: inline-flex; align-items: center; gap: 5px; font-size: var(--fs-xs); color: var(--faint); }

    .chat { display: flex; flex-direction: column; min-height: 0; border-left: 1px solid var(--border); background: var(--surface-1); }
    .main[data-chat='bottom'] .chat { border-left: none; border-top: 1px solid var(--border); }
    .chat-head { height: 48px; flex: none; display: flex; align-items: center; gap: 8px; padding: 0 12px; border-bottom: 1px solid var(--border); font-size: var(--fs-sm); }
    .slow { margin-left: auto; }
    .scroll { flex: 1; min-height: 0; overflow-y: auto; padding: 8px 4px 8px 12px; display: flex; flex-direction: column; gap: 2px; }
    .cm { position: relative; display: flex; gap: 8px; padding: 4px 6px 4px 0; border-radius: var(--r-md); }
    .cm:hover { background: transparent; }
    .cm.sys { align-items: center; gap: 6px; font-size: var(--fs-xs); color: var(--faint); padding-left: 2px; }
    .body { min-width: 0; flex: 0 1 auto; background: var(--surface-2); padding: 7px 12px 8px; border-radius: 16px 16px 16px 5px; }
    .cm { align-items: flex-end; }
    .cm.sys { align-items: center; justify-content: center; }
    .nm { display: flex; align-items: center; gap: 6px; font-size: var(--fs-xs); }
    .nm b { font-weight: var(--fw-semibold); }
    .nm time { color: var(--faint); }
    .tx { font-size: var(--fs-sm); line-height: var(--lh-sm); word-break: break-word; }
    .acts { display: none; position: absolute; right: 4px; top: 2px; gap: 2px; padding: 2px; border-radius: var(--r-md); background: var(--surface-3); border: 1px solid var(--border-strong); }
    .cm:hover .acts, .cm:focus-within .acts { display: flex; }
    .vb { width: 24px; height: 24px; padding: 0; border: none; border-radius: var(--r-sm); background: transparent; color: var(--muted); display: inline-flex; align-items: center; justify-content: center; }
    .vb:hover:not(:disabled) { background: var(--hover); color: var(--danger-fg); border: none; }
    .compose { flex: none; display: flex; align-items: center; gap: 6px; margin: 0 10px 10px; padding: 4px 6px 4px 14px; border-radius: 24px; background: var(--surface-2); }
    .compose input { flex: 1; height: 38px; font-size: var(--fs-md); border: none; background: transparent; box-shadow: none; padding: 0; }
    .legend { padding: 0 12px 6px; font-size: var(--fs-2xs); color: var(--faint); }
    .unread { position: absolute; right: 12px; bottom: 12px; height: 28px; padding: 0 10px; border: none; border-radius: var(--r-md); background: var(--accent); color: var(--accent-ink); font-size: var(--fs-xs); font-weight: var(--fw-semibold); }
</style>
