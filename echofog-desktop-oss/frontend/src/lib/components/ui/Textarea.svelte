<script>
    /** Multi-line field. Use with bind:value; `rows` is the starting height. */
    export let value = '';
    export let label = '';
    export let hint = '';
    export let error = '';
    export let rows = 3;
    export let disabled = false;
    export let id = 'ta-' + Math.random().toString(36).slice(2, 8);
    export let el = undefined;

    $: describedBy = error ? id + '-err' : hint ? id + '-hint' : undefined;
</script>

<div class="field">
    {#if label}<label for={id}>{label}</label>{/if}
    <textarea
        bind:this={el}
        {id}
        {rows}
        {disabled}
        {value}
        class:invalid={!!error}
        aria-invalid={error ? 'true' : undefined}
        aria-describedby={describedBy}
        on:input={(e) => { value = e.currentTarget.value; }}
        on:input
        on:change
        on:keydown
        on:focus
        on:blur
        {...$$restProps}
    ></textarea>
    {#if error}<p id="{id}-err" class="msg err" role="alert">{error}</p>
    {:else if hint}<p id="{id}-hint" class="msg">{hint}</p>{/if}
</div>

<style>
    .field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
    label { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--muted); margin: 0; }
    textarea {
        padding: 8px var(--sp-3); border-radius: var(--r-md); background: var(--surface-2);
        border: 1px solid var(--border); font-size: var(--fs-md); line-height: var(--lh-md); resize: vertical;
        min-height: 60px;
    }
    textarea:hover:not(:disabled) { border-color: var(--border-strong); }
    textarea:focus { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); background: var(--surface-2); }
    textarea.invalid { border-color: var(--danger); }
    textarea:disabled { opacity: 0.5; }
    .msg { margin: 0; font-size: var(--fs-xs); line-height: var(--lh-xs); color: var(--muted); }
    .msg.err { color: var(--danger-fg); }
</style>
