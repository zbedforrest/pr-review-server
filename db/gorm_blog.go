package db

import (
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Blog persistence. Like the finding-outcome methods these are deliberately
// not part of the Database interface; server.blogStore type-asserts for them.

func blogPostFromModel(m *BlogPostModel) BlogPost {
	return BlogPost{
		Slug:        m.Slug,
		Title:       m.Title,
		Dek:         m.Dek,
		AuthorLogin: m.AuthorLogin,
		Published:   m.Published,
		PublishedAt: m.PublishedAt,
		IndexObject: m.IndexObject,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

func blogAssetFromModel(m *BlogAssetModel) BlogAsset {
	return BlogAsset{
		ID:            int(m.ID),
		Slug:          m.Slug,
		Path:          m.Path,
		ContentType:   m.ContentType,
		SizeBytes:     m.SizeBytes,
		StorageObject: m.StorageObject,
		ETag:          m.ETag,
		UploadedAt:    m.UploadedAt,
	}
}

// SaveBlogPost inserts the post or overwrites every field of the existing row
// with the same slug. CreatedAt is kept from the first insert.
func (g *GormDB) SaveBlogPost(p *BlogPost) error {
	model := BlogPostModel{
		Slug:        p.Slug,
		Title:       p.Title,
		Dek:         p.Dek,
		AuthorLogin: p.AuthorLogin,
		Published:   p.Published,
		PublishedAt: p.PublishedAt,
		IndexObject: p.IndexObject,
		CreatedAt:   p.CreatedAt,
	}
	return g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "slug"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"title", "dek", "author_login", "published", "published_at", "index_object", "updated_at",
		}),
	}).Create(&model).Error
}

// GetBlogPost returns the post with the given slug, or nil when there is none.
func (g *GormDB) GetBlogPost(slug string) (*BlogPost, error) {
	var model BlogPostModel
	err := g.db.Where("slug = ?", slug).First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	post := blogPostFromModel(&model)
	return &post, nil
}

// ListBlogPosts returns posts newest first by publication date, falling back
// to creation date for drafts. publishedOnly hides drafts.
func (g *GormDB) ListBlogPosts(publishedOnly bool) ([]BlogPost, error) {
	var models []BlogPostModel
	q := g.db.Order("COALESCE(published_at, created_at) DESC, slug ASC")
	if publishedOnly {
		q = q.Where("published = ?", true)
	}
	if err := q.Find(&models).Error; err != nil {
		return nil, err
	}
	posts := make([]BlogPost, len(models))
	for i := range models {
		posts[i] = blogPostFromModel(&models[i])
	}
	return posts, nil
}

// DeleteBlogPost removes the post row and every asset row of the slug.
func (g *GormDB) DeleteBlogPost(slug string) error {
	return g.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("slug = ?", slug).Delete(&BlogAssetModel{}).Error; err != nil {
			return err
		}
		return tx.Where("slug = ?", slug).Delete(&BlogPostModel{}).Error
	})
}

// UpsertBlogAsset records an uploaded file, replacing the row for the same
// (slug, path) when the file is re-uploaded.
func (g *GormDB) UpsertBlogAsset(a *BlogAsset) error {
	model := BlogAssetModel{
		Slug:          a.Slug,
		Path:          a.Path,
		ContentType:   a.ContentType,
		SizeBytes:     a.SizeBytes,
		StorageObject: a.StorageObject,
		ETag:          a.ETag,
		UploadedAt:    a.UploadedAt,
	}
	return g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "slug"}, {Name: "path"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"content_type", "size_bytes", "storage_object", "etag", "uploaded_at",
		}),
	}).Create(&model).Error
}

// GetBlogAsset returns one file of a post, or nil when there is none.
func (g *GormDB) GetBlogAsset(slug, path string) (*BlogAsset, error) {
	var model BlogAssetModel
	err := g.db.Where("slug = ? AND path = ?", slug, path).First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	asset := blogAssetFromModel(&model)
	return &asset, nil
}

// ListBlogAssets returns every file of a post ordered by path.
func (g *GormDB) ListBlogAssets(slug string) ([]BlogAsset, error) {
	var models []BlogAssetModel
	if err := g.db.Where("slug = ?", slug).Order("path ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	assets := make([]BlogAsset, len(models))
	for i := range models {
		assets[i] = blogAssetFromModel(&models[i])
	}
	return assets, nil
}

// DeleteBlogAsset removes one file row; a missing row is not an error.
func (g *GormDB) DeleteBlogAsset(slug, path string) error {
	return g.db.Where("slug = ? AND path = ?", slug, path).Delete(&BlogAssetModel{}).Error
}
