package garage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/safepath"
	"github.com/dk10ws/ironhide/internal/upload"
)

type Store struct {
	s3     *s3.Client
	bucket string
}

func NewStore(ctx context.Context, c config.Config) (*Store, error) {
	client, err := New(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Store{s3: client, bucket: c.Bucket}, nil
}

func (s *Store) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) (string, error) {
	if err := safepath.ValidateKey(key); err != nil {
		return "", fmt.Errorf("reject key: %w", err)
	}
	if size < 0 {
		return "", fmt.Errorf("size must be known and non-negative, got %d", size)
	}

	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}

	out, err := s.s3.PutObject(ctx, in)
	if err != nil {
		return "", fmt.Errorf("put %s: %w", key, err)
	}
	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

func (s *Store) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if err := safepath.ValidateKey(key); err != nil {
		return nil, 0, fmt.Errorf("reject key: %w", err)
	}
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get %s: %w", key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func (s *Store) ListPrefix(ctx context.Context, prefix string, limit int32) ([]upload.File, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		MaxKeys: aws.Int32(limit),
	}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}

	out, err := s.s3.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}

	files := make([]upload.File, 0, len(out.Contents))
	for _, o := range out.Contents {
		files = append(files, upload.File{
			Key:  aws.ToString(o.Key),
			Size: aws.ToInt64(o.Size),
			ETag: strings.Trim(aws.ToString(o.ETag), `"`),
		})
	}
	return files, nil
}

var _ upload.ObjectStore = (*Store)(nil)
