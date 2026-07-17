package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"wa-gateway/internal/config"
)

// MediaStore persists and serves message media (files) out of band from the
// database. Metadata (mimetype, filename, key) lives in the DB; the bytes live
// here. The interface allows swapping the disk backend for MinIO/S3 later
// without changing callers.
type MediaStore interface {
	// Put stores data under a key derived from session/id/ext and returns the
	// storage key to persist in the DB.
	Put(ctx context.Context, session, id, ext string, data []byte) (key string, err error)
	// Open returns a reader and size for a stored object.
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// Delete removes a stored object (best effort; missing objects are not an error).
	Delete(ctx context.Context, key string) error
}

// newMediaStore builds the configured media backend: "disk" (local filesystem)
// or "s3"/"minio" (S3-compatible object storage). When media storage is
// disabled it always uses a local disk store so no external backend is
// contacted at startup.
func newMediaStore(cfg *config.Config) (MediaStore, error) {
	backend := strings.ToLower(strings.TrimSpace(cfg.MediaBackend))
	if !cfg.StoreMedia {
		backend = "disk"
	}
	switch backend {
	case "", "disk", "local":
		dir := cfg.MediaDir
		if dir == "" {
			dir = filepath.Join(cfg.StoreDir, "media")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create media dir: %w", err)
		}
		return &diskMediaStore{dir: dir}, nil
	case "s3", "minio":
		if cfg.S3Endpoint == "" || cfg.S3Bucket == "" {
			return nil, fmt.Errorf("MEDIA_BACKEND=s3 requires S3_ENDPOINT and S3_BUCKET")
		}
		client, err := minio.New(cfg.S3Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
			Secure: cfg.S3UseSSL,
			Region: cfg.S3Region,
		})
		if err != nil {
			return nil, fmt.Errorf("init s3 client: %w", err)
		}
		// MinIO mungkin belum siap saat gateway start (mis. baru naik bersama).
		// Retry ensure-bucket beberapa kali sebelum menyerah.
		var lastErr error
		for attempt := 1; attempt <= 10; attempt++ {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			exists, err := client.BucketExists(cctx, cfg.S3Bucket)
			if err == nil && !exists {
				err = client.MakeBucket(cctx, cfg.S3Bucket, minio.MakeBucketOptions{Region: cfg.S3Region})
			}
			cancel()
			if err == nil {
				return &s3MediaStore{client: client, bucket: cfg.S3Bucket}, nil
			}
			lastErr = err
			time.Sleep(3 * time.Second)
		}
		return nil, fmt.Errorf("connect s3 bucket %q: %w", cfg.S3Bucket, lastErr)
	default:
		return nil, fmt.Errorf("unsupported MEDIA_BACKEND %q (supported: disk, s3)", cfg.MediaBackend)
	}
}

// mediaObjectKey builds a storage key "<session>/<id><ext>" with safe segments.
func mediaObjectKey(session, id, ext string) string {
	return safeSegment(session) + "/" + safeSegment(id) + normalizeExt(ext)
}

// s3MediaStore stores media in an S3-compatible bucket (e.g. MinIO).
type s3MediaStore struct {
	client *minio.Client
	bucket string
}

func (s *s3MediaStore) Put(ctx context.Context, session, id, ext string, data []byte) (string, error) {
	key := mediaObjectKey(session, id, ext)
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *s3MediaStore) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, 0, err
	}
	return obj, stat.Size, nil
}

func (s *s3MediaStore) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// diskMediaStore stores media files on the local filesystem under dir.
type diskMediaStore struct {
	dir string
}

// safeSegment keeps only characters safe for a filesystem path segment.
func safeSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
}

func normalizeExt(ext string) string {
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return safeSegment(ext)
}

func (d *diskMediaStore) keyFor(session, id, ext string) string {
	return filepath.ToSlash(filepath.Join(safeSegment(session), safeSegment(id)+normalizeExt(ext)))
}

// resolve maps a storage key to an absolute path, guarding against path
// traversal so the result always stays within d.dir.
func (d *diskMediaStore) resolve(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	abs := filepath.Join(d.dir, clean)
	rel, err := filepath.Rel(d.dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid media key")
	}
	return abs, nil
}

func (d *diskMediaStore) Put(_ context.Context, session, id, ext string, data []byte) (string, error) {
	key := d.keyFor(session, id, ext)
	abs, err := d.resolve(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return "", err
	}
	return key, nil
}

func (d *diskMediaStore) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	abs, err := d.resolve(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

func (d *diskMediaStore) Delete(_ context.Context, key string) error {
	abs, err := d.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// extFromMime returns a file extension (with leading dot) for a mimetype,
// stripping any parameters such as "; codecs=opus".
func extFromMime(mimetype string) string {
	if i := strings.IndexByte(mimetype, ';'); i >= 0 {
		mimetype = mimetype[:i]
	}
	mimetype = strings.TrimSpace(mimetype)
	switch mimetype {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "application/pdf":
		return ".pdf"
	}
	if exts, _ := mime.ExtensionsByType(mimetype); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
