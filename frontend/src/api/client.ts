// Base API URL - in dev mode, proxy handles routing to :8080
const API_BASE = import.meta.env.DEV ? '' : '';

export class APIError extends Error {
  constructor(
    message: string,
    public status?: number,
    public statusText?: string
  ) {
    super(message);
    this.name = 'APIError';
  }
}

export async function apiGet<T>(endpoint: string): Promise<T> {
  const response = await fetch(`${API_BASE}${endpoint}`, {
    headers: {
      'Cache-Control': 'no-cache, no-store, must-revalidate',
      'Pragma': 'no-cache',
      'Expires': '0',
    },
  });

  if (!response.ok) {
    throw new APIError(
      `API error: ${response.statusText}`,
      response.status,
      response.statusText
    );
  }

  return response.json();
}

export async function apiPost<T>(endpoint: string, body: unknown): Promise<T> {
  const response = await fetch(`${API_BASE}${endpoint}`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify(body),
  });

  if (!response.ok) {
    throw new APIError(
      `API error: ${response.statusText}`,
      response.status,
      response.statusText
    );
  }

  return response.json();
}

export async function apiDelete<T>(endpoint: string, body: unknown): Promise<T> {
  const response = await fetch(`${API_BASE}${endpoint}`, {
    method: 'DELETE',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify(body),
  });

  if (!response.ok) {
    throw new APIError(
      `API error: ${response.statusText}`,
      response.status,
      response.statusText
    );
  }

  return response.json();
}
