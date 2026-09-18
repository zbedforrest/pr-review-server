import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  BLOG_SLUG_PATTERN,
  blogContentType,
  blogRelativePath,
  deleteBlogPost,
  fetchBlogPosts,
  isHiddenBlogPath,
  saveBlogPost,
  uploadBlogFile,
  type BlogPost,
} from '@/api/blog';
import './settings.scss';

interface UploadProgress {
  path: string;
  status: 'pending' | 'uploading' | 'done' | 'error';
  error?: string;
}

const formatBytes = (n: number) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.ceil(n / 1024)} KB`);
const errorText = (err: unknown) => (err instanceof Error ? err.message : String(err));

export function BlogSection() {
  const queryClient = useQueryClient();
  const posts = useQuery({ queryKey: ['blog-posts'], queryFn: fetchBlogPosts });
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['blog-posts'] });

  const toggle = useMutation({
    mutationFn: (post: BlogPost) =>
      saveBlogPost(post.slug, { title: post.title, dek: post.dek, published: !post.published }),
    onSettled: refresh,
  });
  const remove = useMutation({ mutationFn: deleteBlogPost, onSettled: refresh });

  const [slug, setSlug] = useState('');
  const [title, setTitle] = useState('');
  const [dek, setDek] = useState('');
  const [files, setFiles] = useState<File[]>([]);
  const [folderMode, setFolderMode] = useState(false);
  const [publishAfter, setPublishAfter] = useState(false);
  const [progress, setProgress] = useState<UploadProgress[]>([]);
  const [uploading, setUploading] = useState(false);
  const [formError, setFormError] = useState<string>();
  const fileInput = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const input = fileInput.current;
    if (!input) return;
    if (folderMode) input.setAttribute('webkitdirectory', '');
    else input.removeAttribute('webkitdirectory');
  }, [folderMode]);

  const paths = files.map(blogRelativePath);
  const hasIndex = paths.includes('index.html');
  const slugValid = BLOG_SLUG_PATTERN.test(slug);
  const slugTaken = posts.data?.some((post) => post.slug === slug) ?? false;
  const canCreate = slugValid && !slugTaken && title.trim() !== '' && files.length > 0 && !uploading;
  const willPublish = publishAfter && hasIndex;

  const setStatus = (index: number, patch: Partial<UploadProgress>) =>
    setProgress((current) => current.map((row, i) => (i === index ? { ...row, ...patch } : row)));

  const create = async () => {
    setUploading(true);
    setFormError(undefined);
    setProgress(paths.map((path) => ({ path, status: 'pending' })));
    try {
      await saveBlogPost(slug, { title: title.trim(), dek: dek.trim(), published: false });
      let failed = 0;
      for (const [i, file] of files.entries()) {
        setStatus(i, { status: 'uploading' });
        try {
          await uploadBlogFile(slug, paths[i], file, blogContentType(paths[i], file.type));
          setStatus(i, { status: 'done' });
        } catch (err) {
          failed++;
          setStatus(i, { status: 'error', error: errorText(err) });
        }
      }
      if (willPublish && failed === 0) {
        await saveBlogPost(slug, { title: title.trim(), dek: dek.trim(), published: true });
      }
      if (failed === 0) {
        setSlug('');
        setTitle('');
        setDek('');
        setFiles([]);
        if (fileInput.current) fileInput.current.value = '';
      } else {
        setFormError(`${failed} of ${files.length} files failed; fix them and upload again with the same slug`);
      }
    } catch (err) {
      setFormError(errorText(err));
    } finally {
      setUploading(false);
      refresh();
    }
  };

  const confirmDelete = (post: BlogPost) => {
    if (window.confirm(`Delete "${post.title}" and its ${post.file_count} files?`)) remove.mutate(post.slug);
  };

  return (
    <section className="settings-section" aria-busy={uploading || toggle.isPending || remove.isPending}>
      <div className="settings-section__header">
        <h2 className="settings-section__title">Blog</h2>
        <p className="settings-section__description">
          Posts at <a href="/blog">/blog</a>. A post is a folder: index.html is the page, everything else an asset
          it references by relative path.
        </p>
      </div>
      <div className="settings-section__body">
        {posts.error && (
          <div role="alert" className="settings-section__error">
            Could not load posts: {errorText(posts.error)}
          </div>
        )}
        {posts.data && posts.data.length === 0 && <p className="settings-field__help">No posts yet.</p>}
        {posts.data && posts.data.length > 0 && (
          <ul className="blog-posts">
            {posts.data.map((post) => (
              <li key={post.slug} className="blog-posts__row">
                <div className="blog-posts__info">
                  <a href={post.url} className="blog-posts__title">
                    {post.title}
                  </a>
                  <span className="settings-field__help">
                    {post.slug}, {post.file_count} files, {formatBytes(post.size_bytes)}
                    {post.has_index ? '' : ', no index.html yet'}
                  </span>
                </div>
                <label className="blog-posts__toggle">
                  <input
                    type="checkbox"
                    checked={post.published}
                    disabled={toggle.isPending || (!post.published && !post.has_index)}
                    onChange={() => toggle.mutate(post)}
                    aria-label={`Published: ${post.title}`}
                  />
                  Published
                </label>
                <button
                  type="button"
                  className="settings-section__button"
                  disabled={remove.isPending}
                  onClick={() => confirmDelete(post)}
                  aria-label={`Delete ${post.title}`}
                >
                  Delete
                </button>
              </li>
            ))}
          </ul>
        )}
        {(toggle.error || remove.error) && (
          <div role="alert" className="settings-section__error">
            {errorText(toggle.error ?? remove.error)}
          </div>
        )}

        <fieldset className="blog-new" disabled={uploading}>
          <legend className="settings-field__label">New post</legend>
          <div className="settings-field">
            <label className="settings-field__label" htmlFor="blog-slug">
              Slug
            </label>
            <input
              id="blog-slug"
              className="settings-field__text"
              value={slug}
              onChange={(e) => setSlug(e.target.value.trim().toLowerCase())}
              placeholder="my-first-post"
              aria-invalid={slug !== '' && (!slugValid || slugTaken)}
              aria-describedby={slugTaken ? 'blog-slug-help blog-slug-error' : 'blog-slug-help'}
            />
            <span id="blog-slug-help" className="settings-field__help">
              Lowercase letters, digits and dashes; becomes /blog/&lt;slug&gt;/
            </span>
            {slugTaken && (
              <span id="blog-slug-error" role="alert" className="settings-field__error">
                A post with this slug already exists; delete it here or replace its files with scripts/blog_publish.sh
              </span>
            )}
          </div>
          <div className="settings-field">
            <label className="settings-field__label" htmlFor="blog-title">
              Title
            </label>
            <input id="blog-title" className="settings-field__text" value={title} onChange={(e) => setTitle(e.target.value)} />
          </div>
          <div className="settings-field">
            <label className="settings-field__label" htmlFor="blog-dek">
              Dek
            </label>
            <input
              id="blog-dek"
              className="settings-field__text"
              value={dek}
              onChange={(e) => setDek(e.target.value)}
              placeholder="One-line summary shown on the index"
            />
          </div>
          <div className="settings-field">
            <label className="settings-field__label" htmlFor="blog-files">
              Files
            </label>
            <input
              id="blog-files"
              ref={fileInput}
              type="file"
              multiple
              onChange={(e) =>
                setFiles(Array.from(e.target.files ?? []).filter((file) => !isHiddenBlogPath(blogRelativePath(file))))
              }
              aria-describedby="blog-files-help"
            />
            <span id="blog-files-help" className="settings-field__help">
              {folderMode
                ? 'Pick the post folder; paths inside it are kept'
                : 'Pick index.html and its assets; files land at the top level'}
            </span>
            <div className="settings-field settings-field--inline">
              <input id="blog-folder" type="checkbox" checked={folderMode} onChange={(e) => setFolderMode(e.target.checked)} />
              <label className="settings-field__label" htmlFor="blog-folder">
                Select a folder instead of files
              </label>
            </div>
            {files.length > 0 && !hasIndex && (
              <span className="settings-field__error">No index.html selected; the post cannot be published without one</span>
            )}
          </div>
          <div className="settings-field settings-field--inline">
            <input
              id="blog-publish-after"
              type="checkbox"
              checked={willPublish}
              disabled={!hasIndex}
              onChange={(e) => setPublishAfter(e.target.checked)}
            />
            <label className="settings-field__label" htmlFor="blog-publish-after">
              Publish once every file is uploaded{hasIndex ? '' : ' (needs index.html)'}
            </label>
          </div>
          <div className="settings-section__actions">
            <span role="status" className="settings-section__status">
              {uploading ? 'Uploading' : ''}
            </span>
            <button
              type="button"
              className="settings-section__button settings-section__button--primary"
              disabled={!canCreate}
              onClick={create}
            >
              Create post
            </button>
          </div>
        </fieldset>

        {progress.length > 0 && (
          <ul className="blog-progress" aria-label="Upload progress">
            {progress.map((row) => (
              <li key={row.path} className={`blog-progress__row blog-progress__row--${row.status}`}>
                <span className="blog-progress__path">{row.path}</span>
                <span className="blog-progress__status">{row.error ?? row.status}</span>
              </li>
            ))}
          </ul>
        )}
        {formError && (
          <div role="alert" className="settings-section__error">
            {formError}
          </div>
        )}
      </div>
    </section>
  );
}
