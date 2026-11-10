<script>
    /** Single-line field with an optional label, hint, error and leading icon. Use with bind:value. */
    import Icon from './Icon.svelte';

    export let value = '';
    export let type = 'text';
    export let label = '';
    export let hint = '';
    export let error = '';
    export let icon = '';
    export let size = 'md'; // sm | md | lg
    export let disabled = false;
    export let id = 'in-' + Math.random().toString(36).slice(2, 8);
    export let el = undefined; // the <input>, for focus()

    $: describedBy = error ? id + '-err' : hint ? id + '-hint' : undefined;
</script>

<div class="field">
    {#if label}<label for={id}>{label}</label>{/if}
    <div class="box {size}" class:invalid={!!error} class:disabled>
        {#if icon}<span class="lead"><Icon name={icon} size={16} /></span>{/if}
        <input
            bind:this={el}
            {id}
            {type}
            {disabled}
            {value}
            aria-invalid={error ? 'true' : undefined}
            aria-describedby={describedBy}
            on:input={(e) => { value = e.currentTarget.value; }}
            on:input
            on:change
            on:keydown
            on:focus
            on:blur
            {...$$restProps}
        />
        <slot name="end" />
    </div>
    {#if error}<p id="{id}-err" class="msg err" role="alert">{error}</p>
    {:else if hint}<p id="{id}-hint" class="msg">{hint}</p>{/if}
</div>

<style>
    .field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
    label { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--muted); margin: 0; }
    .box {
        display: flex; align-items: center; gap: 8px; height: var(--ctl-md); padding: 0 var(--sp-3);
        border-radius: var(--r-md); background: var(--surface-2); border: 1px solid var(--border);
        transition: border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
    }
    .box.sm { height: var(--ctl-sm); }
    .box.lg { height: var(--ctl-lg); }
    .box:hover:not(.disabled) { border-color: var(--border-strong); }
    .box:focus-within { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
    .box.invalid { border-color: var(--danger); }
    .box.invalid:focus-within { box-shadow: 0 0 0 3px var(--danger-soft); }
    .box.disabled { opacity: 0.5; }
    .lead { color: var(--faint); display: inline-flex; }
    input {
        flex: 1; min-width: 0; height: 100%; padding: 0; margin: 0; border: 0; background: transparent;
        box-shadow: none; border-radius: 0; font-size: var(--fs-md); color: var(--text);
    }
    input:focus, input:hover { box-shadow: none; border: 0; background: transparent; }
    input::placeholder { color: var(--faint); }
    .msg { margin: 0; font-size: var(--fs-xs); line-height: var(--lh-xs); color: var(--muted); }
    .msg.err { color: var(--danger-fg); }
</style>
