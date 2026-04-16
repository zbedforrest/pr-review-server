// Polling intervals
// WebSockets drive most real-time updates. Keep only low-frequency fallback
// polling where it still adds resilience.
// 0 means disabled for React Query refetchInterval.
export const PR_POLL_INTERVAL = 0; 
export const STATUS_POLL_INTERVAL = 60000; // Conservative fallback for status snapshots
export const PRIORITY_POLL_INTERVAL = 0;
export const HEALTH_POLL_INTERVAL = 0;

// React Query stale time
export const PR_STALE_TIME = 15000; 
export const STATUS_STALE_TIME = 5000;
export const PRIORITY_STALE_TIME = 30000;
