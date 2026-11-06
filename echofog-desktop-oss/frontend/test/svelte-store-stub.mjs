export function writable(value) {
  const subs = new Set();
  return {
    set(v) { value = v; subs.forEach((f) => f(value)); },
    update(fn) { this.set(fn(value)); },
    subscribe(f) { subs.add(f); f(value); return () => subs.delete(f); },
  };
}
export function get(store) { let v; store.subscribe((x) => (v = x))(); return v; }
