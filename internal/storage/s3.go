package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"gookorbit/internal/config"
)

type Service struct {
	client  *s3.Client
	bucket  string
	presign bool
	logger  *slog.Logger
}

func New(ctx context.Context, cfg config.S3, log *slog.Logger) (*Service, error) {
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		if cfg.UseSSL {
			endpoint = "https://" + endpoint
		} else {
			endpoint = "http://" + endpoint
		}
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = cfg.ForcePathStyle
	})
	return &Service{client: client, bucket: cfg.Bucket, presign: cfg.StoragePresign, logger: log}, nil
}

func BookKey(bookID uuid.UUID, format string) string {
	return fmt.Sprintf("books/%s/original.%s", bookID, format)
}

func CoverKey(bookID uuid.UUID) string {
	return fmt.Sprintf("books/%s/cover.jpg", bookID)
}

func (s *Service) PutObject(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        r,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// GetObject streams an object without buffering it fully in memory.
func (s *Service) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get %s: %w", key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func (s *Service) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	pc := s3.NewPresignClient(s.client)
	req, err := pc.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) {
		o.Expires = expiry
	})
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *Service) PresignEnabled() bool { return s.presign }

func (s *Service) DeletePrefix(ctx context.Context, prefix string) error {
	var ids []types.ObjectIdentifier
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range out.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: o.Key})
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.bucket),
		Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", prefix, err)
	}
	s.logger.Info("deleted book assets", "prefix", prefix, "count", len(ids))
	return nil
}

func (s *Service) CreateBucketIfMissing(ctx context.Context) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		if _, hErr := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); hErr == nil {
			return nil // already exists
		}
		return fmt.Errorf("create bucket %s: %w", s.bucket, err)
	}
	s.logger.Info("created s3 bucket", "bucket", s.bucket)
	return nil
}
