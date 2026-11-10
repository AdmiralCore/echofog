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
    import Button from '../ui/Button.svelte';
    import Badge from '../ui/Badge.svelte';
    import Input from '../ui/Input.svelte';
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
        <span class="ic" class:warn={!!path.hurts}><Icon name={path.relayed ? 'relay' : 'check'} size={16} /></span>
        <div class="grow">
            <b>{$t(path.relayed ? 'callpath.title' : 'callpath.titleDirect')}</b>
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

    {#if path.relayed}
        <p class="sub">{$t('callpath.why')}</p>
        <p class="text">{$t('callpath.reason.' + path.reason)}</p>
    {/if}
    {#if sides}
        <div class="sides">
            <span>{$t('callpath.side.mine')}: <b>{$t('callpath.nat.' + mySide(path))}</b></span>
            {#if theirNat}<span>{$t('callpath.side.theirs')}: <b>{$t('callpath.nat.' + theirNat)}</b></span>{/if}
        </div>
    {/if}

    {#if fixes.length}
        <p class="sub">{$t('callpath.helps')}</p>
        <ol class="fixes">
            {#each fixes as f}
                <li>{f === 'forward' && !path.listenPort ? $t('callpath.fix.forwardAny') : $t('callpath.fix.' + f, { port: path.listenPort })}</li>
            {/each}
        </ol>
    {/if}

    {#if group}
        <p class="sub">{$t('callpath.people')}</p>
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
            <p class="sub">{$t('callpath.relays')}</p>
            {#if path.relayed}
                <Button size="sm" icon="refresh" loading={busy === 'switch'} disabled={!!busy || free.length < 2} on:click={() => switchTo('')}>
                    {busy === 'switch' ? $t('callpath.switching') : $t('callpath.switch')}
                </Button>
            {/if}
        </div>
        <p class="hint">{$t('callpath.relaysHint')}</p>
        {#if relays === null}
            <p class="hint"><Icon name="spinner" size={13} /> {$t('common.loading')}</p>
        {:else if !free.length}
            <p class="hint">{$t('callpath.relaysEmpty')}</p>
        {/if}
        {#each free as r (r.peerId)}
            <div class="relay" class:sel={r.carrying > 0}>
                <span class="mono label" title={r.addr}>{relayHost(r.addr, r.peerId)}</span>
                {#if r.mine}<Badge size="sm">{$t('callpath.mine')}</Badge>{/if}
                {#if r.guard}<Badge size="sm">{$t('callpath.guard')}</Badge>{/if}
                <span class="rate">{r.pingMs ? $t('callpath.ms', { ms: r.pingMs }) : $t('callpath.noAnswer')}</span>
                {#if r.pricePerMb}<span class="rate">{price(r.pricePerMb)}</span>{/if}
                {#if r.carrying > 0}
                    <Badge variant="accent" size="sm">{$t('callpath.carrying')}</Badge>
                {:else if path.relayed}
                    <Button size="sm" loading={busy === r.peerId} disabled={!!busy} on:click={() => switchTo(r.peerId)}>{busy === r.peerId ? $t('callpath.switching') : $t('callpath.use')}</Button>
                {/if}
            </div>
        {/each}
        {#if error}<p class="err">{error}</p>{/if}

        <p class="sub">{$t('callpath.paid')}</p>
        {#if paid.length}
            <p class="hint">{$t('callpath.paidHint')}</p>
            {#each paid as r (r.peerId)}
                <div class="relay">
                    <span class="mono label">{relayHost(r.addr, r.peerId)}</span>
                    <span class="rate">{price(r.pricePerMb)}</span>
                    <span title={$t('callpath.soonTitle')}><Badge size="sm">{$t('callpath.soon')}</Badge></span>
                </div>
            {/each}
        {:else if relays !== null}
            <p class="hint">{$t('callpath.paidNone')}</p>
        {/if}

        <p class="sub">{$t('callpath.addTitle')}</p>
        <div class="add">
            <div class="grow" on:keydown={(e) => e.key === 'Enter' && addRelay()}><Input size="sm" bind:value={link} placeholder="echofog://relay/host:port/peer-id" aria-label={$t('callpath.addTitle')} /></div>
            <Button variant="primary" size="sm" loading={busy === 'add'} disabled={!!busy || !link.trim()} on:click={addRelay}>{busy === 'add' ? $t('callpath.adding') : $t('callpath.add')}</Button>
        </div>
        {#if linkError}<p class="err">{linkError}</p>{:else if added}<p class="hint ok">{$t('callpath.added')}</p>{/if}
    {/if}
</div>

<style>
    .path { display: flex; flex-direction: column; gap: var(--sp-2); padding: var(--sp-2) var(--sp-1) var(--sp-2); }
    .head { display: flex; gap: var(--sp-3); align-items: flex-start; padding: var(--sp-1) var(--sp-2) 0; }
    .head b { font-size: var(--fs-md); font-weight: var(--fw-semibold); }
    .head p { margin-top: 2px; color: var(--muted); font-size: var(--fs-sm); line-height: 1.4; }
    .ic { width: 30px; height: 30px; flex: none; border-radius: var(--r-md); display: grid; place-items: center; background: var(--accent-soft); color: var(--accent-fg); }
    .ic.warn { background: var(--warn-soft); color: var(--warn-fg); }
    .route, .member, .relay { display: flex; align-items: center; gap: var(--sp-2); padding: 7px 10px; border-radius: var(--r-md); font-size: var(--fs-sm); }
    .route { background: var(--surface-2); margin: 0 var(--sp-2); }
    .relay.sel { background: var(--accent-soft); }
    .label { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .rate { color: var(--muted); font-variant-numeric: tabular-nums; font-size: var(--fs-xs); white-space: nowrap; }
    .hurt { margin: 2px var(--sp-2); padding: 8px 10px; border-radius: var(--r-md); background: var(--warn-soft); color: var(--warn-fg); font-size: var(--fs-sm); line-height: 1.4; }
    .text { margin: 0 var(--sp-3); color: var(--muted); font-size: var(--fs-sm); line-height: 1.45; }
    .sub { margin: var(--sp-2) var(--sp-3) 0; font-size: var(--fs-2xs); font-weight: var(--fw-bold); letter-spacing: 0.06em; text-transform: uppercase; color: var(--faint); }
    .sides { display: flex; flex-direction: column; gap: 2px; margin: 2px var(--sp-3) 0; font-size: var(--fs-xs); color: var(--faint); }
    .sides b { color: var(--muted); font-weight: var(--fw-semibold); }
    .fixes { margin: 0 var(--sp-3) 0 28px; padding: 0; list-style: decimal; display: flex; flex-direction: column; gap: 6px; color: var(--muted); font-size: var(--fs-sm); line-height: 1.4; }
    .fixes li::marker { color: var(--accent-fg); font-weight: var(--fw-bold); }
    .dot { width: 8px; height: 8px; flex: none; border-radius: 999px; background: var(--faint); }
    .dot.via-direct { background: var(--ok); }
    .dot.via-relay { background: var(--accent); }
    .sect { display: flex; align-items: center; justify-content: space-between; gap: var(--sp-2); padding-right: var(--sp-2); }
    .sect .sub { margin-bottom: 0; }
    .hint { margin: 0 var(--sp-3); font-size: var(--fs-xs); line-height: 1.4; color: var(--faint); display: flex; gap: 6px; align-items: center; }
    .hint.ok { color: var(--ok-fg); }
    .err { margin: 0 var(--sp-3); font-size: var(--fs-xs); color: var(--danger-fg); }
    .add { display: flex; gap: var(--sp-2); padding: 0 var(--sp-2); align-items: flex-start; }
    .add .grow { min-width: 0; }
    .faint { flex: none; color: var(--faint); }
    .mono { font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: var(--fs-xs); }
</style>
