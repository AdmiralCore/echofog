<script>
    /** Native drop-down (fully keyboard and screen-reader friendly) in the system's field look. */
    export let value = '';
    export let options = []; // [{ value, label, disabled? }]
    export let label = '';
    export let hint = '';
    export let size = 'md';
    export let disabled = false;
    export let id = 'sel-' + Math.random().toString(36).slice(2, 8);
</script>

<div class="field">
    {#if label}<label for={id}>{label}</label>{/if}
    <select
        {id}
        {disabled}
        class={size}
        aria-describedby={hint ? id + '-hint' : undefined}
        {value}
        on:change={(e) => { value = e.currentTarget.value; }}
        on:change
        {...$$restProps}
    >
        {#each options as o (o.value)}
            <option value={o.value} disabled={o.disabled} selected={o.value === value}>{o.label}</option>
        {/each}
    </select>
    {#if hint}<p id="{id}-hint" class="msg">{hint}</p>{/if}
</div>

<style>
    .field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
    label { font-size: var(--fs-sm); font-weight: var(--fw-medium); color: var(--muted); margin: 0; }
    select { height: var(--ctl-md); padding: 0 32px 0 var(--sp-3); font-size: var(--fs-md); border-radius: var(--r-md); border-color: var(--border); }
    select.sm { height: var(--ctl-sm); }
    select.lg { height: var(--ctl-lg); }
    select:disabled { opacity: 0.5; cursor: default; }
    .msg { margin: 0; font-size: var(--fs-xs); line-height: var(--lh-xs); color: var(--muted); }
</style>
