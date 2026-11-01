/**
 * A picture made small before it is kept or sent: the middle square of the image, size px wide,
 * as JPEG base64 (without the "data:" head). With maxBytes the quality steps down until it fits.
 */
export async function squarePicture(file, size = 256, maxBytes = 0) {
  const img = await createImageBitmap(file);
  const c = document.createElement('canvas');
  c.width = c.height = size;
  const s = Math.min(img.width, img.height);
  c.getContext('2d').drawImage(img, (img.width - s) / 2, (img.height - s) / 2, s, s, 0, 0, size, size);
  for (const q of [0.85, 0.75, 0.62, 0.5, 0.38]) {
    const data = c.toDataURL('image/jpeg', q);
    const b64 = data.slice(data.indexOf(',') + 1);
    if (!maxBytes || (b64.length * 3) / 4 <= maxBytes) return b64;
  }
  throw new Error('the picture stays too large');
}
