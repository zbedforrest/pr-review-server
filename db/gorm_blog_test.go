package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGormDB_BlogPost_RoundTripAndOverwrite(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	require.NoError(t, db.SaveBlogPost(&BlogPost{Slug: "hello", Title: "Hello", Dek: "First", AuthorLogin: "alice"}))

	got, err := db.GetBlogPost("hello")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Hello", got.Title)
	assert.Equal(t, "First", got.Dek)
	assert.Equal(t, "alice", got.AuthorLogin)
	assert.False(t, got.Published)
	assert.Nil(t, got.PublishedAt)
	assert.False(t, got.CreatedAt.IsZero())

	now := time.Now().UTC().Truncate(time.Second)
	got.Title = "Hello again"
	got.Published = true
	got.PublishedAt = &now
	got.IndexObject = "blog/hello/index.html"
	require.NoError(t, db.SaveBlogPost(got))

	again, err := db.GetBlogPost("hello")
	require.NoError(t, err)
	assert.Equal(t, "Hello again", again.Title)
	assert.True(t, again.Published)
	require.NotNil(t, again.PublishedAt)
	assert.WithinDuration(t, now, *again.PublishedAt, time.Second)
	assert.Equal(t, "blog/hello/index.html", again.IndexObject)
	assert.WithinDuration(t, got.CreatedAt, again.CreatedAt, time.Second)

	missing, err := db.GetBlogPost("nope")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestGormDB_ListBlogPosts_OrderAndDraftFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	require.NoError(t, db.SaveBlogPost(&BlogPost{Slug: "old", Title: "Old", Published: true, PublishedAt: &old}))
	require.NoError(t, db.SaveBlogPost(&BlogPost{Slug: "new", Title: "New", Published: true, PublishedAt: &recent}))
	require.NoError(t, db.SaveBlogPost(&BlogPost{Slug: "draft", Title: "Draft"}))

	all, err := db.ListBlogPosts(false)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "draft", all[0].Slug, "drafts sort by creation time, which is newest here")
	assert.Equal(t, "new", all[1].Slug)
	assert.Equal(t, "old", all[2].Slug)

	published, err := db.ListBlogPosts(true)
	require.NoError(t, err)
	require.Len(t, published, 2)
	assert.Equal(t, "new", published[0].Slug)
	assert.Equal(t, "old", published[1].Slug)
}

func TestGormDB_BlogAsset_RoundTripReplaceAndDeleteWithPost(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	require.NoError(t, db.SaveBlogPost(&BlogPost{Slug: "hello", Title: "Hello"}))
	uploaded := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, db.UpsertBlogAsset(&BlogAsset{
		Slug: "hello", Path: "img/a.png", ContentType: "image/png", SizeBytes: 10,
		StorageObject: "blog/hello/img/a.png", ETag: `"abc"`, UploadedAt: uploaded,
	}))
	require.NoError(t, db.UpsertBlogAsset(&BlogAsset{
		Slug: "hello", Path: "index.html", ContentType: "text/html", SizeBytes: 5,
		StorageObject: "blog/hello/index.html", ETag: `"idx"`, UploadedAt: uploaded,
	}))

	got, err := db.GetBlogAsset("hello", "img/a.png")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "image/png", got.ContentType)
	assert.EqualValues(t, 10, got.SizeBytes)
	assert.Equal(t, `"abc"`, got.ETag)
	assert.WithinDuration(t, uploaded, got.UploadedAt, time.Second)

	require.NoError(t, db.UpsertBlogAsset(&BlogAsset{
		Slug: "hello", Path: "img/a.png", ContentType: "image/png", SizeBytes: 20,
		StorageObject: "blog/hello/img/a.png", ETag: `"def"`, UploadedAt: uploaded,
	}))
	assets, err := db.ListBlogAssets("hello")
	require.NoError(t, err)
	require.Len(t, assets, 2, "re-uploading a path replaces its row")
	assert.Equal(t, "img/a.png", assets[0].Path)
	assert.EqualValues(t, 20, assets[0].SizeBytes)
	assert.Equal(t, "index.html", assets[1].Path)

	missing, err := db.GetBlogAsset("hello", "img/zzz.png")
	require.NoError(t, err)
	assert.Nil(t, missing)

	require.NoError(t, db.DeleteBlogAsset("hello", "img/a.png"))
	require.NoError(t, db.DeleteBlogAsset("hello", "img/a.png"), "deleting twice is fine")
	assets, err = db.ListBlogAssets("hello")
	require.NoError(t, err)
	require.Len(t, assets, 1)
	assert.Equal(t, "index.html", assets[0].Path)

	require.NoError(t, db.DeleteBlogPost("hello"))
	post, err := db.GetBlogPost("hello")
	require.NoError(t, err)
	assert.Nil(t, post)
	assets, err = db.ListBlogAssets("hello")
	require.NoError(t, err)
	assert.Empty(t, assets)
}
