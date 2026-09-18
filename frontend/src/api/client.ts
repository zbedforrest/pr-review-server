// Base API URL - in dev mode, proxy handles routing to :8080
const API_BASE = import.meta.env.DEV ? '' : '';

export class APIError extends Error {
  constructor(
    message: string,
    public status?: number,
    public statusText?: string,
    /** Machine-readable code from endpoints that answer with a JSON error body. */
    public code?: string,
    public details?: Record<string, string>
  ) {
    super(message);
    this.name = 'APIError';
  }
}

function handleUnauthorized(response: Response): void {
  if (response.status === 401) {
    window.location.href = '/login';
  }
}

// Go handlers answer with one short line; anything else (a proxy's HTML
// error page) is not worth showing.
const MAX_ERROR_BODY = 500;

interface JSONErrorBody {
  error: string;
  code?: string;
  details?: Record<string, string>;
}

function parseJSONError(body: string): JSONErrorBody | null {
  if (!body.startsWith('{')) return null;
  try {
    const parsed: unknown = JSON.parse(body);
    if (typeof parsed !== 'object' || parsed === null) return null;
    const { error, code, details } = parsed as Record<string, unknown>;
    if (typeof error !== 'string') return null;
    return {
      error,
      code: typeof code === 'string' ? code : undefined,
      details: typeof details === 'object' && details !== null ? (details as Record<string, string>) : undefined,
    };
  } catch {
    return null;
  }
}

async function errorFromResponse(response: Response): Promise<APIError> {
  const body = (await response.text().catch(() => '')).trim();
  const fallback = `API error: ${response.statusText || response.status}`;
  const json = parseJSONError(body);
  if (json) {
    return new APIError(json.error || fallback, response.status, response.statusText, json.code, json.details);
  }
  const readable = body !== '' && body.length <= MAX_ERROR_BODY && !body.startsWith('<');
  return new APIError(readable ? body : fallback, response.status, response.statusText);
}

export async function apiGet<T>(endpoint: string): Promise<T> {
  const response = await fetch(`${API_BASE}${endpoint}`, {
    headers: {
      'Cache-Control': 'no-cache, no-store, must-revalidate',
      'Pragma': 'no-cache',
      'Expires': '0',
    },
  });

  handleUnauthorized(response);

  if (!response.ok) {
    throw await errorFromResponse(response);
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

  handleUnauthorized(response);

  if (!response.ok) {
    throw await errorFromResponse(response);
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

  handleUnauthorized(response);

  if (!response.ok) {
    throw await errorFromResponse(response);
  }

  return response.json();
}
