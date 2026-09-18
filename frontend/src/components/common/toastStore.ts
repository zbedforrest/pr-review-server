export type ToastTone = 'success' | 'danger' | 'neutral';

export interface ToastOptions {
  title: string;
  /** External link shown after the title, opened in a new tab. */
  href?: string;
  hrefLabel?: string;
}

export interface ToastItem extends ToastOptions {
  id: number;
  tone: ToastTone;
}

export const TOAST_DURATION_MS = 5000;

type Listener = (items: ToastItem[]) => void;

// Module-level queue so any component can raise a toast while the single
// <ToastHost /> in App renders them.
let items: ToastItem[] = [];
let nextId = 1;
const listeners = new Set<Listener>();
const timers = new Map<number, ReturnType<typeof setTimeout>>();

function emit() {
  for (const listener of listeners) listener(items);
}

function dismiss(id: number) {
  const timer = timers.get(id);
  if (timer) clearTimeout(timer);
  timers.delete(id);
  if (!items.some((t) => t.id === id)) return;
  items = items.filter((t) => t.id !== id);
  emit();
}

function show(tone: ToastTone, opts: ToastOptions): number {
  const id = nextId++;
  items = [...items, { id, tone, ...opts }];
  timers.set(id, setTimeout(() => dismiss(id), TOAST_DURATION_MS));
  emit();
  return id;
}

export const toast = {
  success: (opts: ToastOptions) => show('success', opts),
  error: (opts: ToastOptions) => show('danger', opts),
  info: (opts: ToastOptions) => show('neutral', opts),
  dismiss,
  subscribe(listener: Listener): () => void {
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  },
  getSnapshot: () => items,
  /** Test helper: drop every toast and timer. */
  clearAll() {
    for (const timer of timers.values()) clearTimeout(timer);
    timers.clear();
    items = [];
    emit();
  },
};
