<script>
  // Which country the route policy assumes (V2-FIELD-1). Automatic takes it from the public
  // address (GeoIP); "Do not specify" keeps the strictest row; a country is the user's own answer.
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import { t, lang } from '../i18n/index.js';
  import Icon from './ui/Icon.svelte';

  // ISO 3166-1 alpha-2. The visible names come from Intl.DisplayNames, so they follow the UI
  // language without a second translation table.
  const CODES = (
    'AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ ' +
    'CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR ' +
    'GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP ' +
    'KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT ' +
    'MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW ' +
    'SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG ' +
    'UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'
  ).split(' ');

  let st = null; // { protocol, code, source, detected }
  let busy = false;
  let saved = false;
  let error = '';

  $: names = new Intl.DisplayNames([$lang || 'en'], { type: 'region' });
  $: options = CODES.map((cc) => ({ cc, name: names.of(cc) || cc }))
    .sort((a, b) => a.name.localeCompare(b.name));

  async function load() {
    try {
      st = await App.GetCountry();
    } catch (err) {
      error = errText(err);
    }
  }

  async function pick(value) {
    if (busy || value === (st?.code || '')) return;
    busy = true;
    saved = false;
    error = '';
    try {
      await App.SetCountry(value);
      saved = true;
      await load();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  onMount(load);
</script>

{#if st && st.protocol === 2}
  <div class="card stack" id="country-card">
    <div class="card-head">
      <span class="ico"><Icon name="globe" size={19} /></span>
      <div class="grow">
        <h2>{$t('country.title')}</h2>
        <p class="small muted">{$t('country.hint')}</p>
      </div>
    </div>
    {#if st.source === 'unknown' && !st.code}
      <p class="small warn" role="status">{$t('country.unknown')}</p>
    {/if}
    <label class="small muted" for="country-select">{$t('country.label')}</label>
    <select id="country-select" class="country" value={st.code || ''} disabled={busy} on:change={(e) => pick(e.currentTarget.value)}>
      <option value="">{$t('country.auto')}</option>
      <option value="none">{$t('country.none')}</option>
      {#each options as o}
        <option value={o.cc}>{o.cc} — {o.name}</option>
      {/each}
    </select>
    {#if st.code && st.code !== 'none'}
      <p class="small muted">{$t('country.selected', { code: st.code })}</p>
    {:else if st.source === 'auto' && st.detected}
      <p class="small muted">{$t('country.detected', { code: st.detected })}</p>
    {/if}
    {#if saved}<p class="small ok" role="status">{$t('country.saved')}</p>{/if}
    {#if error}<p class="small error" role="alert">{error}</p>{/if}
  </div>
{/if}

<style>
  .country {
    width: 100%; padding: 9px 11px; border: 1px solid var(--border-strong); border-radius: var(--r-md);
    background: var(--surface-2); color: var(--text); font: inherit;
  }
  .country:disabled { opacity: .6; }
</style>
