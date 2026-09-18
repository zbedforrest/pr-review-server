import { apiDelete, apiGet, apiPut, APIError } from './client';

export interface BlogPost {
  slug: string;
  title: string;
  dek: string;
  author_login: string;
  published: boolean;
  published_at: string | null;
  created_at: string;
  updated_at: string;
  has_index: boolean;
  file_count: number;
  size_bytes: number;
  url: string;
}

export interface BlogFile {
  path: string;
  content_type: string;
  size_bytes: number;
  uploaded_at: string;
}

export interface BlogPostInput {
  title: string;
  dek: string;
  published: boolean;
}

export const BLOG_SLUG_PATTERN = /^[a-z0-9][a-z0-9-]{1,80}$/;

const CONTENT_TYPES: Record<string, string> = {
  html: 'text/html',
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
  svg: 'image/svg+xml',
  webm: 'video/webm',
  mp4: 'video/mp4',
  css: 'text/css',
  woff2: 'font/woff2',
  json: 'application/json',
  txt: 'text/plain',
};

export async function fetchBlogPosts(): Promise<BlogPost[]> {
  const response = await apiGet<{ posts: BlogPost[] }>('/api/blog/posts');
  return response.posts;
}

export async function saveBlogPost(slug: string, input: BlogPostInput): Promise<BlogPost> {
  const response = await apiPut<{ post: BlogPost }>(`/api/blog/posts/${encodeURIComponent(slug)}`, input);
  return response.post;
}

export async function deleteBlogPost(slug: string): Promise<void> {
  await apiDelete(`/api/blog/posts/${encodeURIComponent(slug)}`, undefined);
}

export async function uploadBlogFile(slug: string, path: string, file: Blob, contentType: string): Promise<BlogFile> {
  const encodedPath = path.split('/').map(encodeURIComponent).join('/');
  const response = await fetch(`/api/blog/posts/${encodeURIComponent(slug)}/files/${encodedPath}`, {
    method: 'PUT',
    headers: { 'Content-Type': contentType },
    body: file,
  });
  if (!response.ok) {
    const body = (await response.text().catch(() => '')).trim();
    throw new APIError(body || `API error: ${response.status}`, response.status, response.statusText);
  }
  const result = (await response.json()) as { file: BlogFile };
  return result.file;
}

/** Content type by extension, falling back to what the browser reports. */
export function blogContentType(path: string, reported: string): string {
  const ext = path.slice(path.lastIndexOf('.') + 1).toLowerCase();
  return CONTENT_TYPES[ext] ?? reported ?? '';
}

/** Post-relative path of a picked file; a folder pick drops the folder itself. */
export function blogRelativePath(file: File): string {
  const nested = file.webkitRelativePath;
  if (nested) {
    const slash = nested.indexOf('/');
    return slash >= 0 ? nested.slice(slash + 1) : nested;
  }
  return file.name;
}
