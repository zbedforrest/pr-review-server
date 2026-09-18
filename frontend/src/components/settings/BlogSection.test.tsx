import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { BlogPost } from '@/api/blog';
import { BlogSection } from './BlogSection';

const fetchMock = vi.fn();

const livePost: BlogPost = {
  slug: 'hello',
  title: 'Hello',
  dek: 'First post',
  author_login: 'alice',
  published: true,
  published_at: '2026-09-01T00:00:00Z',
  created_at: '2026-09-01T00:00:00Z',
  updated_at: '2026-09-01T00:00:00Z',
  has_index: true,
  file_count: 3,
  size_bytes: 2 * 1024 * 1024,
  url: '/blog/hello/',
};

const draftWithoutIndex: BlogPost = {
  ...livePost,
  slug: 'draft',
  title: 'Draft',
  published: false,
  published_at: null,
  has_index: false,
  file_count: 0,
  size_bytes: 0,
  url: '/blog/draft/',
};

const jsonResponse = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Call = { url: string; method: string; contentType: string | null; body: unknown };

const calls = (): Call[] =>
  fetchMock.mock.calls.map(([url, init]) => {
    const request = (init ?? {}) as RequestInit;
    const headers = new Headers(request.headers);
    return { url: url as string, method: request.method ?? 'GET', contentType: headers.get('Content-Type'), body: request.body };
  });

const renderSection = () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <BlogSection />
    </QueryClientProvider>
  );
};

describe('BlogSection', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('lists posts with a publish toggle that saves the flipped flag', async () => {
    let posts = [livePost, draftWithoutIndex];
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts }));
      if (url === '/api/blog/posts/hello' && init?.method === 'PUT') {
        posts = [{ ...livePost, published: false }, draftWithoutIndex];
        return Promise.resolve(jsonResponse({ post: posts[0] }));
      }
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    renderSection();

    const link = await screen.findByRole('link', { name: 'Hello' });
    expect(link.getAttribute('href')).toBe('/blog/hello/');
    expect(screen.getByText(/hello, 3 files, 2\.0 MB/)).toBeTruthy();
    expect(screen.getByText(/no index\.html yet/)).toBeTruthy();

    const draftToggle = screen.getByRole('checkbox', { name: 'Published: Draft' }) as HTMLInputElement;
    expect(draftToggle.disabled).toBe(true);

    fireEvent.click(screen.getByRole('checkbox', { name: 'Published: Hello' }));
    await waitFor(() => expect(calls().some((c) => c.method === 'PUT')).toBe(true));
    const put = calls().find((c) => c.method === 'PUT')!;
    expect(put.url).toBe('/api/blog/posts/hello');
    expect(JSON.parse(put.body as string)).toEqual({ title: 'Hello', dek: 'First post', published: false });
    await waitFor(() => expect((screen.getByRole('checkbox', { name: 'Published: Hello' }) as HTMLInputElement).checked).toBe(false));
  });

  it('asks before deleting a post', async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts: [livePost] }));
      if (init?.method === 'DELETE') return Promise.resolve(jsonResponse({ status: 'deleted' }));
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    renderSection();

    fireEvent.click(await screen.findByRole('button', { name: 'Delete Hello' }));
    expect(confirm).toHaveBeenCalled();
    expect(calls().some((c) => c.method === 'DELETE')).toBe(false);

    confirm.mockReturnValue(true);
    fireEvent.click(screen.getByRole('button', { name: 'Delete Hello' }));
    await waitFor(() => expect(calls().some((c) => c.method === 'DELETE')).toBe(true));
    expect(calls().find((c) => c.method === 'DELETE')!.url).toBe('/api/blog/posts/hello');
  });

  it('creates the post, uploads every file with its type and path, then publishes', async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts: [] }));
      if (init?.method === 'PUT' && url.includes('/files/')) {
        return Promise.resolve(jsonResponse({ file: { path: 'x', content_type: 'x', size_bytes: 1, uploaded_at: '' } }));
      }
      if (init?.method === 'PUT') {
        const body = JSON.parse(init.body as string);
        return Promise.resolve(jsonResponse({ post: { ...livePost, slug: 'new-post', ...body } }, 201));
      }
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    renderSection();
    await screen.findByText('No posts yet.');

    const createButton = screen.getByRole('button', { name: 'Create post' }) as HTMLButtonElement;
    expect(createButton.disabled).toBe(true);

    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'New-Post' } });
    fireEvent.change(screen.getByLabelText('Title'), { target: { value: 'A new post' } });
    fireEvent.change(screen.getByLabelText('Dek'), { target: { value: 'Summary' } });
    const index = new File(['<p>hi</p>'], 'index.html', { type: 'text/html' });
    const image = new File(['png'], 'hero.png', { type: '' });
    Object.defineProperty(image, 'webkitRelativePath', { value: 'site/img/hero.png' });
    fireEvent.change(screen.getByLabelText('Files'), { target: { files: [index, image] } });
    fireEvent.click(screen.getByLabelText(/Publish once every file is uploaded/));

    expect(createButton.disabled).toBe(false);
    fireEvent.click(createButton);

    await waitFor(() => expect(calls().filter((c) => c.method === 'PUT').length).toBe(4));
    const puts = calls().filter((c) => c.method === 'PUT');
    expect(puts[0].url).toBe('/api/blog/posts/new-post');
    expect(JSON.parse(puts[0].body as string)).toEqual({ title: 'A new post', dek: 'Summary', published: false });
    expect(puts[1].url).toBe('/api/blog/posts/new-post/files/index.html');
    expect(puts[1].contentType).toBe('text/html');
    expect(puts[2].url).toBe('/api/blog/posts/new-post/files/img/hero.png');
    expect(puts[2].contentType).toBe('image/png');
    expect(puts[2].body).toBe(image);
    expect(JSON.parse(puts[3].body as string)).toEqual({ title: 'A new post', dek: 'Summary', published: true });

    expect(screen.getByText('index.html').nextElementSibling?.textContent).toBe('done');
    expect(screen.getByText('img/hero.png').nextElementSibling?.textContent).toBe('done');
    expect((screen.getByLabelText('Slug') as HTMLInputElement).value).toBe('');
  });

  it('refuses a slug that already exists and drops hidden files from a folder pick', async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts: [livePost] }));
      if (init?.method === 'PUT' && url.includes('/files/')) {
        return Promise.resolve(jsonResponse({ file: { path: 'x', content_type: 'x', size_bytes: 1, uploaded_at: '' } }));
      }
      if (init?.method === 'PUT') return Promise.resolve(jsonResponse({ post: { ...livePost, slug: 'hello-2' } }, 201));
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    renderSection();
    await screen.findByRole('link', { name: 'Hello' });

    fireEvent.change(screen.getByLabelText('Title'), { target: { value: 'Again' } });
    const index = new File(['<p>hi</p>'], 'index.html', { type: 'text/html' });
    Object.defineProperty(index, 'webkitRelativePath', { value: 'site/index.html' });
    const junk = new File([''], '.DS_Store', { type: '' });
    Object.defineProperty(junk, 'webkitRelativePath', { value: 'site/img/.DS_Store' });
    fireEvent.change(screen.getByLabelText('Files'), { target: { files: [index, junk] } });

    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'hello' } });
    expect(screen.getByRole('alert').textContent).toContain('already exists');
    expect((screen.getByRole('button', { name: 'Create post' }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'hello-2' } });
    expect(screen.queryByRole('alert')).toBeNull();
    const publishToggle = screen.getByLabelText(/Publish once every file is uploaded/) as HTMLInputElement;
    expect(publishToggle.disabled).toBe(false);
    fireEvent.click(publishToggle);
    fireEvent.click(screen.getByRole('button', { name: 'Create post' }));

    await waitFor(() => expect(calls().filter((c) => c.method === 'PUT').length).toBe(3));
    const urls = calls().filter((c) => c.method === 'PUT').map((c) => c.url);
    expect(urls).toEqual(['/api/blog/posts/hello-2', '/api/blog/posts/hello-2/files/index.html', '/api/blog/posts/hello-2']);
  });

  it('cannot ask to publish without an index.html', async () => {
    fetchMock.mockImplementation((url: string) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts: [] }));
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    renderSection();
    await screen.findByText('No posts yet.');

    const publishToggle = screen.getByLabelText(/Publish once every file is uploaded/) as HTMLInputElement;
    expect(publishToggle.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText('Files'), { target: { files: [new File(['png'], 'hero.png', { type: 'image/png' })] } });
    expect(publishToggle.disabled).toBe(true);
    expect(screen.getByText(/No index\.html selected/)).toBeTruthy();
  });

  it('keeps the form and reports a failed upload without publishing', async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts: [] }));
      if (init?.method === 'PUT' && url.endsWith('/files/run.js')) {
        return Promise.resolve(new Response('content type application/javascript is not allowed for assets', { status: 400 }));
      }
      if (init?.method === 'PUT' && url.includes('/files/')) {
        return Promise.resolve(jsonResponse({ file: { path: 'x', content_type: 'x', size_bytes: 1, uploaded_at: '' } }));
      }
      if (init?.method === 'PUT') return Promise.resolve(jsonResponse({ post: livePost }, 201));
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    renderSection();
    await screen.findByText('No posts yet.');

    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'broken' } });
    fireEvent.change(screen.getByLabelText('Title'), { target: { value: 'Broken' } });
    const index = new File(['<p>hi</p>'], 'index.html', { type: 'text/html' });
    const script = new File(['alert(1)'], 'run.js', { type: 'application/javascript' });
    fireEvent.change(screen.getByLabelText('Files'), { target: { files: [index, script] } });
    fireEvent.click(screen.getByLabelText(/Publish once every file is uploaded/));
    fireEvent.click(screen.getByRole('button', { name: 'Create post' }));

    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('1 of 2 files failed'));
    expect(screen.getByText('run.js').nextElementSibling?.textContent).toContain('not allowed');
    const metaPuts = calls().filter((c) => c.method === 'PUT' && !c.url.includes('/files/'));
    expect(metaPuts.length).toBe(1);
    expect((screen.getByLabelText('Slug') as HTMLInputElement).value).toBe('broken');
  });

  it('lets the same form retry the draft it created after a failure', async () => {
    let posts: BlogPost[] = [];
    let scriptRejected = false;
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/blog/posts') return Promise.resolve(jsonResponse({ posts }));
      if (init?.method === 'PUT' && url.endsWith('/files/run.css') && !scriptRejected) {
        scriptRejected = true;
        return Promise.resolve(new Response('storage unavailable', { status: 500 }));
      }
      if (init?.method === 'PUT' && url.includes('/files/')) {
        return Promise.resolve(jsonResponse({ file: { path: 'x', content_type: 'x', size_bytes: 1, uploaded_at: '' } }));
      }
      if (init?.method === 'PUT') {
        posts = [{ ...livePost, slug: 'retry', title: 'Retry', published: false, has_index: true }];
        return Promise.resolve(jsonResponse({ post: posts[0] }, 201));
      }
      return Promise.resolve(new Response('not found', { status: 404 }));
    });
    renderSection();
    await screen.findByText('No posts yet.');

    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'retry' } });
    fireEvent.change(screen.getByLabelText('Title'), { target: { value: 'Retry' } });
    const files = [new File(['<p>hi</p>'], 'index.html', { type: 'text/html' }), new File(['a{}'], 'run.css', { type: 'text/css' })];
    fireEvent.change(screen.getByLabelText('Files'), { target: { files } });
    fireEvent.click(screen.getByRole('button', { name: 'Create post' }));

    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('1 of 2 files failed'));
    await screen.findByRole('link', { name: 'Retry' });
    const createButton = screen.getByRole('button', { name: 'Create post' }) as HTMLButtonElement;
    expect(createButton.disabled).toBe(false);
    expect(screen.queryByText(/already exists/)).toBeNull();

    fireEvent.click(createButton);
    await waitFor(() => expect((screen.getByLabelText('Slug') as HTMLInputElement).value).toBe(''));
    expect(calls().filter((c) => c.method === 'PUT' && c.url.endsWith('/files/run.css')).length).toBe(2);
    expect(screen.getByText('run.css').nextElementSibling?.textContent).toBe('done');
  });
});
