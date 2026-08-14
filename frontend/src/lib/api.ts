export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** Same-origin JSON fetch. The session cookie is HttpOnly, so there is no token
 *  to attach here — the browser does it. */
export async function api<T = unknown>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body !== undefined) headers.set('content-type', 'application/json');

  const res = await fetch(path, { credentials: 'same-origin', ...init, headers });
  const text = await res.text();
  let data: any = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { error: text };
    }
  }
  if (!res.ok) throw new ApiError(res.status, data?.error ?? res.statusText);
  return data as T;
}

export const get = <T>(p: string) => api<T>(p);
export const post = <T>(p: string, body?: unknown) =>
  api<T>(p, { method: 'POST', body: JSON.stringify(body ?? {}) });
export const patch = <T>(p: string, body: unknown) =>
  api<T>(p, { method: 'PATCH', body: JSON.stringify(body) });
export const del = <T>(p: string) => api<T>(p, { method: 'DELETE' });

export type Me = { id: string; name: string; admin: boolean; created: string };

export type Site = {
  id: string;
  slug: string;
  domain: string;
  owner: string;
  owner_name: string;
  members: string[];
  collaborators: { id: string; name: string }[];
  files: number;
  bytes: number;
  version: number;
  deployed?: string;
  mine: boolean;
  created: string;
};

export type Token = {
  id: string;
  name: string;
  created: string;
  last_used?: string;
};

export type Invite = {
  code: string;
  creator: string;
  created: string;
  used_by?: string;
  used_at?: string;
};

export type Manifest = {
  version: number;
  updated: string;
  files: Record<string, { hash: string; size: number }>;
};

export function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

export function ago(iso?: string): string {
  if (!iso) return 'never';
  const t = new Date(iso).getTime();
  if (!t) return 'never';
  const s = Math.floor((Date.now() - t) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
