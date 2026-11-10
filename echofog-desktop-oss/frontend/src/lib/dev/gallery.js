// Development-only entry of the component gallery (`?mock&gallery`, see main.js).
import { addStrings } from '../i18n/index.js';
import strings from './gallery-strings.js';
import Gallery from './Gallery.svelte';

export default function mountGallery(target) {
  addStrings(strings);
  return new Gallery({ target });
}
