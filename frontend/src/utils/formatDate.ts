export function formatDate(dateStr: string | null): string {
  if (!dateStr) return 'Not yet reviewed';
  const date = new Date(dateStr);
  return date.toLocaleString();
}

export function formatTime(dateStr: string): string {
  const date = new Date(dateStr);
  return date.toLocaleTimeString();
}

/**
 * Compact relative time, e.g. "just now", "5m ago", "2h ago", "3d ago".
 * Falls back to a locale date string for anything older than a week.
 */
export function formatRelativeTime(dateStr: string | null): string {
  if (!dateStr) return 'never';
  const then = new Date(dateStr).getTime();
  if (Number.isNaN(then)) return 'unknown';

  const seconds = Math.floor((Date.now() - then) / 1000);
  if (seconds < 45) return 'just now';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 7) return `${days}d ago`;
  return new Date(then).toLocaleDateString();
}
