package garage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

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

func (s *Store) PutObject(ctx context.Context, key string, body io.Reader, size int64) (string, error) {
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

// maxDeleteBatch is the S3 limit on keys in one DeleteObjects request.
const maxDeleteBatch = 1000

// maxDeleteObjects caps how many keys one DeletePrefix call will remove.
//
// It is a circuit breaker, not a correctness limit: an unbounded delete could
// run for a very long time holding a request open. A caller that hits the cap
// gets a short read rather than a surprise.
const maxDeleteObjects = 10000

// DeletePrefix removes every object under prefix and returns how many it
// deleted.
//
// Deleting a prefix takes two calls because S3 has no "delete a directory":
// the keys must be listed, then deleted in batches. Both steps paginate,
// because a single ListObjectsV2 page holds at most 1000 keys and a single
// DeleteObjects request at most 1000 too.
//
// Deleting a key that is already gone is not an error, so this is safe to
// retry after a partial failure.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := safepath.ValidateKey(prefix); err != nil {
		return 0, fmt.Errorf("reject prefix: %w", err)
	}

	total := 0

	for {
		if total >= maxDeleteObjects {
			return total, fmt.Errorf("delete prefix %s: stopped at the %d object cap", prefix, total)
		}

		var token *string
		batch := make([]types.ObjectIdentifier, 0, maxDeleteBatch)

		for len(batch) < maxDeleteBatch {
			out, err := s.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket:            aws.String(s.bucket),
				Prefix:            aws.String(prefix),
				ContinuationToken: token,
			})
			if err != nil {
				return total, fmt.Errorf("list %s: %w", prefix, err)
			}

			for _, o := range out.Contents {
				batch = append(batch, types.ObjectIdentifier{
					Key: aws.String(aws.ToString(o.Key)),
				})
			}

			if !aws.ToBool(out.IsTruncated) || aws.ToString(out.NextContinuationToken) == "" {
				token = nil
				break
			}
			token = out.NextContinuationToken
		}

		if len(batch) == 0 {
			return total, nil
		}

		// Quiet keeps Garage from writing one access record per deleted key.
		res, err := s.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return total, fmt.Errorf("delete %s: %w", prefix, err)
		}
		// A partial failure comes back as a 200 with per-key errors, so the
		// call itself succeeding is not enough.
		if len(res.Errors) > 0 {
			return total + len(batch) - len(res.Errors),
				fmt.Errorf("delete %s: %d of %d keys failed, first: %s %s",
					prefix, len(res.Errors), len(batch),
					aws.ToString(res.Errors[0].Code), aws.ToString(res.Errors[0].Message))
		}

		total += len(batch)
	}
}
