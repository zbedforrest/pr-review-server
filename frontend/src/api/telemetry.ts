import { apiPost, apiGet } from './client';

export interface TelemetryEventPayload {
  action: string;
  label?: string;
  pr_owner?: string;
  pr_repo?: string;
  pr_number?: number;
}

export interface TelemetryStats {
  total_events: number;
  active_users: number;
  by_action: { action: string; count: number }[];
  by_day: { date: string; count: number }[];
  top_searches: { label: string; count: number }[];
  top_prs: { owner: string; repo: string; number: number; count: number }[];
}

export async function trackEvents(events: TelemetryEventPayload[]): Promise<void> {
  await apiPost('/api/telemetry/track', { events });
}

export async function fetchTelemetryStats(days: number): Promise<TelemetryStats> {
  return apiGet<TelemetryStats>(`/api/telemetry/stats?days=${days}`);
}
