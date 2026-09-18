import { useSyncExternalStore } from 'react';
import { toast, type ToastItem } from './toastStore';
import './Toast.scss';

function ToastCard({ item }: { item: ToastItem }) {
  return (
    <div className={`toast toast--${item.tone}`} onClick={() => toast.dismiss(item.id)}>
      <span className="toast__title">{item.title}</span>
      {item.href && (
        <>
          <span className="toast__sep" aria-hidden="true">·</span>
          <a className="toast__link" href={item.href} target="_blank" rel="noopener noreferrer">
            {item.hrefLabel ?? 'View on GitHub'} ↗
          </a>
        </>
      )}
    </div>
  );
}

/** Bottom-right toast stack. Mount once, in App. */
export function ToastHost() {
  const items = useSyncExternalStore(toast.subscribe, toast.getSnapshot, toast.getSnapshot);
  return (
    <div className="toast-host" role="status" aria-live="polite">
      {items.map((item) => (
        <ToastCard key={item.id} item={item} />
      ))}
    </div>
  );
}
