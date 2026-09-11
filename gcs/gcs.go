package gcs

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// Client wraps the GCS storage client
type Client struct {
	bucket     *storage.BucketHandle
	bucketName string
}

// NewClient creates a new GCS client for the specified bucket
func NewClient(ctx context.Context, bucketName string) (*Client, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	return &Client{
		bucket:     client.Bucket(bucketName),
		bucketName: bucketName,
	}, nil
}

// ReviewFileName generates a filename for a review based on PR info and commit SHA.
// Format: {owner}_{repo}_{prNumber}_{commitSHA}.html
func ReviewFileName(owner, repo string, prNumber int, commitSHA string) string {
	shortSHA := commitSHA
	if len(commitSHA) > 7 {
		shortSHA = commitSHA[:7]
	}
	return fmt.Sprintf("%s_%s_%d_%s.html", owner, repo, prNumber, shortSHA)
}

// ReviewJSONFileName returns the sidecar JSON object name for a given HTML
// review filename. The .json sidecar holds the structured findings payload
// served by /api/review; the .html object is unchanged and continues to be
// the browser-rendered view.
func ReviewJSONFileName(htmlFilename string) string {
	if strings.HasSuffix(htmlFilename, ".html") {
		return strings.TrimSuffix(htmlFilename, ".html") + ".json"
	}
	return htmlFilename + ".json"
}

// ReviewRunFileName returns the immutable HTML object path for one execution.
// The canonical ReviewFileName remains the latest alias for a PR+commit.
func ReviewRunFileName(owner, repo string, prNumber int, commitSHA, runID string) string {
	shortSHA := commitSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	return fmt.Sprintf("runs/%s/%s/%d/%s/%s.html", owner, repo, prNumber, shortSHA, runID)
}

// ReviewRunJSONFileName is the immutable structured sidecar paired with a
// ReviewRunFileName object.
func ReviewRunJSONFileName(owner, repo string, prNumber int, commitSHA, runID string) string {
	return ReviewJSONFileName(ReviewRunFileName(owner, repo, prNumber, commitSHA, runID))
}

// archiveOnOverwrite reports whether overwritten review artifacts should be
// preserved under archive/ first (REVIEW_HISTORY_ARCHIVE=true). Read per call
// so tests can toggle it; unset keeps today's overwrite-in-place behavior.
//
// Rationale: review objects are per-SHA, so re-pushes never collide — but a
// force re-review of the SAME SHA overwrites its artifacts in place, which
// has permanently destroyed human-scored baselines before. Archiving the old
// generation costs one server-side copy on the rare manual re-review.
func archiveOnOverwrite() bool {
	return os.Getenv("REVIEW_HISTORY_ARCHIVE") == "true"
}

// archiveExisting best-effort copies the current generation of filename to
// archive/<filename>.<updated-unix-ts> before an overwrite. archive/ objects
// never match ListReviewsForPR's prefix, so history stays out of listings.
// Failures are logged and swallowed — archival must never block a review.
func (c *Client) archiveExisting(ctx context.Context, filename string) {
	if !archiveOnOverwrite() {
		return
	}
	src := c.bucket.Object(filename)
	attrs, err := src.Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return // first write, nothing to preserve
	}
	if err != nil {
		log.Printf("[GCS] WARN: archive check for %s failed: %v", filename, err)
		return
	}
	archiveName := fmt.Sprintf("archive/%s.%d", filename, attrs.Updated.Unix())
	if _, err := c.bucket.Object(archiveName).CopierFrom(src).Run(ctx); err != nil {
		log.Printf("[GCS] WARN: archive copy %s -> %s failed: %v", filename, archiveName, err)
		return
	}
	log.Printf("[GCS] Archived overwritten object: gs://%s/%s", c.bucketName, archiveName)
}

// UploadReview uploads review content to GCS and returns the object path.
func (c *Client) UploadReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, content []byte) (string, error) {
	filename := ReviewFileName(owner, repo, prNumber, commitSHA)

	c.archiveExisting(ctx, filename)

	obj := c.bucket.Object(filename)
	writer := obj.NewWriter(ctx)
	writer.ContentType = "text/html; charset=utf-8"

	// Reviews used to be considered immutable per commit, but the manual
	// trigger now force-overwrites the same filename when the user clicks
	// "Review" — so browsers must revalidate to pick up the new content.
	// `no-cache` doesn't mean "don't store"; it means "always revalidate
	// before using". GCS serves an ETag, so revalidation is a cheap 304.
	writer.CacheControl = "public, no-cache, must-revalidate"

	if _, err := writer.Write(content); err != nil {
		writer.Close()
		return "", fmt.Errorf("failed to write to GCS: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to close GCS writer: %w", err)
	}

	log.Printf("[GCS] Uploaded review: gs://%s/%s", c.bucketName, filename)
	return filename, nil
}

// UploadReviewSidecar uploads an arbitrary object (e.g. the .json findings
// sidecar) under the given filename with the given content type. Same cache
// policy as UploadReview so a regenerated review's sidecar is picked up by
// callers on next fetch.
func (c *Client) UploadReviewSidecar(ctx context.Context, filename, contentType string, content []byte) error {
	return c.uploadReviewArtifact(ctx, filename, contentType, content, true)
}

// UploadImmutableReviewArtifact writes a run-scoped artifact without archive
// lookup: run IDs are unique, so the object never intentionally overwrites.
func (c *Client) UploadImmutableReviewArtifact(ctx context.Context, filename, contentType string, content []byte) error {
	return c.uploadReviewArtifact(ctx, filename, contentType, content, false)
}

func (c *Client) uploadReviewArtifact(ctx context.Context, filename, contentType string, content []byte, archive bool) error {
	if archive {
		c.archiveExisting(ctx, filename)
	}

	obj := c.bucket.Object(filename)
	writer := obj.NewWriter(ctx)
	writer.ContentType = contentType
	if archive {
		writer.CacheControl = "public, no-cache, must-revalidate"
	} else {
		writer.CacheControl = "public, max-age=31536000, immutable"
	}

	if _, err := writer.Write(content); err != nil {
		writer.Close()
		return fmt.Errorf("failed to write review artifact to GCS: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close GCS review artifact writer: %w", err)
	}

	log.Printf("[GCS] Uploaded review artifact: gs://%s/%s", c.bucketName, filename)
	return nil
}

// ReviewExists checks if a review already exists for a given PR and commit.
func (c *Client) ReviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error) {
	filename := ReviewFileName(owner, repo, prNumber, commitSHA)

	obj := c.bucket.Object(filename)
	_, err := obj.Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check object existence: %w", err)
	}

	return true, nil
}

// GetReviewURLByFilename returns the public URL for a review file by filename
func (c *Client) GetReviewURLByFilename(filename string) string {
	if filename == "" {
		return ""
	}
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s", c.bucketName, filename)
}

// ListReviewsForPR lists all reviews for a given PR (all commits)
func (c *Client) ListReviewsForPR(ctx context.Context, owner, repo string, prNumber int) ([]ReviewInfo, error) {
	prefix := fmt.Sprintf("%s_%s_%d_", owner, repo, prNumber)

	var reviews []ReviewInfo
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		// Filename format: {owner}_{repo}_{prNumber}_{commitSHA}.html
		name := attrs.Name
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		trimmed := strings.TrimSuffix(name, ".html")

		parts := strings.Split(trimmed, "_")
		if len(parts) < 4 {
			continue
		}
		commitSHA := parts[len(parts)-1]

		reviews = append(reviews, ReviewInfo{
			Filename:  name,
			CommitSHA: commitSHA,
			CreatedAt: attrs.Created,
			URL:       fmt.Sprintf("https://storage.googleapis.com/%s/%s", c.bucketName, name),
		})
	}

	return reviews, nil
}

// GetLatestReviewForPR returns the most recent review for a PR, if any
func (c *Client) GetLatestReviewForPR(ctx context.Context, owner, repo string, prNumber int) (*ReviewInfo, error) {
	reviews, err := c.ListReviewsForPR(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, err
	}

	if len(reviews) == 0 {
		return nil, nil
	}

	// Find the most recent review
	latest := reviews[0]
	for _, r := range reviews[1:] {
		if r.CreatedAt.After(latest.CreatedAt) {
			latest = r
		}
	}

	return &latest, nil
}

// ReviewInfo contains metadata about a review stored in GCS
type ReviewInfo struct {
	Filename  string
	CommitSHA string
	CreatedAt time.Time
	URL       string
}

// GetReviewContent downloads and returns the content of a review file
func (c *Client) GetReviewContent(ctx context.Context, filename string) ([]byte, error) {
	obj := c.bucket.Object(filename)
	reader, err := obj.NewReader(ctx)
	if err == storage.ErrObjectNotExist {
		return nil, fmt.Errorf("review not found: %s", filename)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read review: %w", err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read review content: %w", err)
	}

	return content, nil
}

// BucketName returns the bucket name
func (c *Client) BucketName() string {
	return c.bucketName
}
