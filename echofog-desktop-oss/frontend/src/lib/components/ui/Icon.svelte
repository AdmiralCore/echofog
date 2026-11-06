<script>
    /**
     * The one icon set: Lucide (ISC), stroke icons on a 24x24 grid, drawn in currentColor.
     * Only the icons the app uses are bundled (lib/icons.js); add one by name there.
     * Sizes: 16 inside controls and lists, 20 in toolbars.
     */
    import { ICONS } from '../../icons.js';

    export let name;
    export let size = 20;
    export let stroke = 1.5;
    export let className = '';
    export let fill = 'none';
    /** a label for an icon that stands alone; without it the icon is hidden from screen readers */
    export let label = '';

    $: body = ICONS[name] || '';
</script>

<svg
    class="shrink-0 {className}"
    class:spin={name === 'spinner'}
    width={size}
    height={size}
    viewBox="0 0 24 24"
    {fill}
    stroke="currentColor"
    stroke-width={stroke}
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden={label ? undefined : 'true'}
    role={label ? 'img' : undefined}
    aria-label={label || undefined}
>{@html body}</svg>

<style>
    .spin { animation: spin 0.9s linear infinite; }
</style>
