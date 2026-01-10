export function formatDate(dateStr: string | null): string {
  if (!dateStr) return 'Not yet reviewed';
  const date = new Date(dateStr);
  return date.toLocaleString();
}

export function formatTime(dateStr: string): string {
  const date = new Date(dateStr);
  return date.toLocaleTimeString();
}
