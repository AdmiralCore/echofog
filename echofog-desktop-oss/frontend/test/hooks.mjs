export async function resolve(specifier, context, next) {
  if (specifier === 'svelte/store') return { url: new URL('./svelte-store-stub.mjs', import.meta.url).href, shortCircuit: true };
  return next(specifier, context);
}
