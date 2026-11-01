<script>
    /**
     * Why the call goes through a relay and what helps, for this side's case only: behind the
     * provider's NAT router settings cannot help, behind a closed router they do, and a relay the
     * user chose gets no advice. Then the relays the call can use (each pinged when the panel
     * opens), switching to another one, the paid ones (shown, not sold yet) and adding one by link.
     */
    import { onMount, createEventDispatcher } from 'svelte';
    import { App, errText } from '../../api.js';
    import { callService } from '../../services/call.service.js';
    import { peerName } from '../../stores/peers.store.js';
    import { parseRelayLink } from '../../contact.js';
    import { price } from '../../format.js';
    import { chosen, fixesFor, leadOf, mySide, relayHost } from '../../callpath.js';
    import Icon from '../ui/Icon.svelte';
    import { t, tr } from '../../i18n/index.js';

    export let path;

    const dispatch = createEventDispatcher();
    let relays = null;
    let busy = ''; // '' | 'switch' | <relay peer id> | 'add'
    let error = '';
    let link = '';
    let linkError = '';
    let added = false;

    $: group = path.members.length > 1;
    $: one = path.members.find((m) => m.link === 'relay');
    $: lead = leadOf(path);
    $: onPurpose = chosen(path.reason);
    $: fixes = fixesFor(path.reason);
    $: sides = !onPurpose && path.reason !== 'checking';
    $: theirNat = !group ? one?.theirNat : '';
    $: free = (relays || []).filter((r) => r.connected);
    $: paid = (relays || []).filter((r) => !r.connected);
    const mbps = (kbps) => (kbps / 1000).toFixed(1);

    async function loadRelays() {
        try {
            relays = await App.GetCallRelays();
        } catch (err) {
            relays = [];
            error = errText(err);
        }
    }

    async function switchTo(via = '') {
        busy = via || 'switch';
        error = '';
        try {
            await App.SwitchCallRelay(via);
            callService.notify(tr('callpath.switched'));
            dispatch('changed');
            await loadRelays();
        } catch (err) {
            error = errText(err);
        } finally {
            busy = '';
        }
    }

    async function addRelay() {
        linkError = '';
        added = false;
        if (!parseRelayLink(link)) {
            linkError = $t('relay.badLink');
            return;
        }
        busy = 'add';
        try {
            await App.AddRelay(link.trim());
            link = '';
            added = true;
            await loadRelays();
            setTimeout(loadRelays, 3000); // the relay is dialed now: it shows once connected
        } catch (err) {
            linkError = errText(err);
        } finally {
            busy = '';
        }
    }

    onMount(() => {
        if (path.reason !== 'anonymous') loadRelays();
    });
</script>

<div class="path">
    <div class="head">
        <span class="ic" class:warn={!!path.hurts}><Icon name="relay" size={16} /></span>
        <div class="grow">
            <b>{$t('callpath.title')}</b>
            <p>{$t(lead.key, lead.params)}</p>
        </div>
    </div>

    {#if !group && one}
        <div class="route">
            <span class="faint">{$t('callpath.via')}</span>
            <span class="mono label">{relayHost(one.relayAddr, one.relay)}</span>
            {#if one.pingMs}<span class="rate">{$t('callpath.ms', { ms: one.pingMs })}</span>{/if}
        </div>
    {/if}

    {#if path.hurts === 'ping'}
        <p class="hurt">{$t('callpath.hurts.ping', { ms: path.pingMs })}</p>
    {:else if path.hurts === 'capped'}
        <p class="hurt">{$t('callpath.hurts.capped', { now: mbps(path.sendKbps), preset: mbps(path.presetKbps) })}</p>
    {/if}

    <p class="pop-title">{$t('callpath.why')}</p>
    <p class="text">{$t('callpath.reason.' + path.reason)}</p>
    {#if sides}
        <div class="sides">
            <span>{$t('callpath.side.mine')}: <b>{$t('callpath.nat.' + mySide(path))}</b></span>
            {#if theirNat}<span>{$t('callpath.side.theirs')}: <b>{$t('callpath.nat.' + theirNat)}</b></span>{/if}
        </div>
    {/if}

    {#if fixes.length}
        <p class="pop-title">{$t('callpath.helps')}</p>
        <ol class="fixes">
            {#each fixes as f}
                <li>{f === 'forward' && !path.listenPort ? $t('callpath.fix.forwardAny') : $t('callpath.fix.' + f, { port: path.listenPort })}</li>
            {/each}
        </ol>
    {/if}

    {#if group}
        <p class="pop-title">{$t('callpath.people')}</p>
        {#each path.members as m (m.peer)}
            <div class="member">
                <span class="dot" class:via-relay={m.link === 'relay'} class:via-direct={m.link === 'direct'}></span>
                <span class="label">{$peerName(m.peer)}</span>
                <span class="rate">
                    {#if m.link === 'relay'}{m.pingMs ? $t('callpath.link.relayPing', { ms: m.pingMs }) : $t('callpath.link.relay')}
                    {:else if m.link === 'direct'}{$t('callpath.link.direct')}
                    {:else}{$t('callpath.link.none')}{/if}
                </span>
            </div>
        {/each}
    {/if}

    {#if path.reason !== 'anonymous'}
        <div class="sect">
            <p class="pop-title">{$t('callpath.relays')}</p>
            <button class="switch" disabled={!!busy || free.length < 2} on:click={() => switchTo('')}>
                <Icon name="refresh" size={13} /> {busy === 'switch' ? $t('callpath.switching') : $t('callpath.switch')}
            </button>
        </div>
        <p class="hint">{$t('callpath.relaysHint')}</p>
        {#if relays === null}
            <p class="pop-empty"><span class="spinner"></span> {$t('common.loading')}</p>
        {:else if !free.length}
            <p class="pop-empty">{$t('callpath.relaysEmpty')}</p>
        {/if}
        {#each free as r (r.peerId)}
            <div class="relay" class:sel={r.carrying > 0}>
                <span class="mono label" title={r.addr}>{relayHost(r.addr, r.peerId)}</span>
                {#if r.mine}<span class="chip">{$t('callpath.mine')}</span>{/if}
                {#if r.guard}<span class="chip">{$t('callpath.guard')}</span>{/if}
                <span class="rate">{r.pingMs ? $t('callpath.ms', { ms: r.pingMs }) : $t('callpath.noAnswer')}</span>
                {#if r.pricePerMb}<span class="rate">{price(r.pricePerMb)}</span>{/if}
                {#if r.carrying > 0}
                    <span class="chip on">{$t('callpath.carrying')}</span>
                {:else}
                    <button class="use" disabled={!!busy} on:click={() => switchTo(r.peerId)}>{busy === r.peerId ? $t('callpath.switching') : $t('callpath.use')}</button>
                {/if}
            </div>
        {/each}
        {#if error}<p class="err">{error}</p>{/if}

        <p class="pop-title">{$t('callpath.paid')}</p>
        {#if paid.length}
            <p class="hint">{$t('callpath.paidHint')}</p>
            {#each paid as r (r.peerId)}
                <div class="relay">
                    <span class="mono label">{relayHost(r.addr, r.peerId)}</span>
                    <span class="rate">{price(r.pricePerMb)}</span>
                    <span class="chip" title={$t('callpath.soonTitle')}>{$t('callpath.soon')}</span>
                </div>
            {/each}
        {:else if relays !== null}
            <p class="hint">{$t('callpath.paidNone')}</p>
        {/if}

        <p class="pop-title">{$t('callpath.addTitle')}</p>
        <div class="add">
            <input class="grow" bind:value={link} placeholder="echofog://relay/host:port/peer-id" on:keydown={(e) => e.key === 'Enter' && addRelay()} />
            <button class="primary" disabled={!!busy || !link.trim()} on:click={addRelay}>{busy === 'add' ? $t('callpath.adding') : $t('callpath.add')}</button>
        </div>
        {#if linkError}<p class="err">{linkError}</p>{:else if added}<p class="hint ok">{$t('callpath.added')}</p>{/if}
    {/if}
</div>

<style>
    .path { display: flex; flex-direction: column; gap: 6px; padding: 6px 4px 8px; }
    .head { display: flex; gap: 10px; align-items: flex-start; padding: 4px 6px 2px; }
    .head b { font-size: 14px; }
    .head p { margin-top: 2px; color: var(--muted); line-height: 1.4; }
    .ic { width: 30px; height: 30px; flex: none; border-radius: 10px; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent); }
    .ic.warn { background: var(--warn-soft); color: var(--warn); }
    .route, .member, .relay { display: flex; align-items: center; gap: 8px; padding: 7px 10px; border-radius: 9px; }
    .route { background: var(--raised); margin: 0 6px; }
    .relay.sel { background: color-mix(in srgb, var(--accent) 12%, transparent); }
    .hurt { margin: 2px 6px; padding: 8px 10px; border-radius: 9px; background: var(--warn-soft); color: var(--warn); line-height: 1.4; }
    .text { margin: 0 10px; color: var(--muted); line-height: 1.45; }
    .sides { display: flex; flex-direction: column; gap: 2px; margin: 2px 10px 0; font-size: 12px; color: var(--faint); }
    .sides b { color: var(--muted); font-weight: 600; }
    .fixes { margin: 0 10px 0 28px; padding: 0; list-style: decimal; display: flex; flex-direction: column; gap: 6px; color: var(--muted); line-height: 1.4; }
    .fixes li::marker { color: var(--accent); font-weight: 700; }
    .dot { width: 8px; height: 8px; flex: none; border-radius: 999px; background: var(--faint); }
    .dot.via-direct { background: var(--ok); }
    .dot.via-relay { background: var(--accent); }
    .sect { display: flex; align-items: center; justify-content: space-between; padding-right: 6px; }
    .switch { display: inline-flex; align-items: center; gap: 5px; height: 26px; padding: 0 10px; font-size: 12px; }
    .hint { margin: 0 10px; font-size: 11.5px; line-height: 1.4; color: var(--faint); }
    .hint.ok { color: var(--ok); }
    .err { margin: 0 10px; font-size: 12px; color: var(--danger); }
    .chip { flex: none; padding: 1px 7px; border-radius: 999px; font-size: 10.5px; background: var(--raised); color: var(--muted); }
    .chip.on { background: var(--accent-soft); color: var(--accent); }
    .use { flex: none; height: 24px; padding: 0 9px; font-size: 11.5px; }
    .add { display: flex; gap: 6px; padding: 0 6px; }
    .add input { min-width: 0; height: 32px; padding: 4px 10px; font-size: 12px; }
    .add button { height: 32px; padding: 0 12px; font-size: 12px; }
    .faint { flex: none; color: var(--faint); }
    .mono { font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: 12px; }
</style>
