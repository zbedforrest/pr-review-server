package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pr-review-server/config"
	"pr-review-server/gcs"
)

var errBlogObjectNotFound = errors.New("blog object not found")

// blogObjectStore holds the uploaded files of every post under
// blog/<slug>/<path>: the review-artifact bucket when one is configured,
// otherwise a local directory so dev mode works without GCS.
type blogObjectStore interface {
	Put(ctx context.Context, key, contentType string, content []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

func newBlogObjectStore(cfg *config.Config, gcsClient *gcs.Client) blogObjectStore {
	if gcsClient != nil && gcsClient.BucketName() != "" {
		return gcsBlogObjects{client: gcsClient}
	}
	dir := cfg.BlogLocalDir
	if dir == "" {
		dir = "./data/blog"
	}
	return localBlogObjects{dir: dir}
}

func blogObjectKey(slug, path string) string {
	return "blog/" + slug + "/" + path
}

type gcsBlogObjects struct{ client *gcs.Client }

func (g gcsBlogObjects) Put(ctx context.Context, key, contentType string, content []byte) error {
	return g.client.UploadObject(ctx, key, contentType, content)
}

func (g gcsBlogObjects) Get(ctx context.Context, key string) ([]byte, error) {
	content, err := g.client.ReadObject(ctx, key)
	if errors.Is(err, gcs.ErrObjectNotFound) {
		return nil, errBlogObjectNotFound
	}
	return content, err
}

func (g gcsBlogObjects) Delete(ctx context.Context, key string) error {
	return g.client.DeleteObject(ctx, key)
}

type localBlogObjects struct{ dir string }

func (l localBlogObjects) filePath(key string) (string, error) {
	root, err := filepath.Abs(l.dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(key))
	if !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("blog object key %q escapes %s", key, root)
	}
	return full, nil
}

func (l localBlogObjects) Put(_ context.Context, key, _ string, content []byte) error {
	full, err := l.filePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), full)
}

func (l localBlogObjects) Get(_ context.Context, key string) ([]byte, error) {
	full, err := l.filePath(key)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return nil, errBlogObjectNotFound
	}
	return content, err
}

func (l localBlogObjects) Delete(_ context.Context, key string) error {
	full, err := l.filePath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
