// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectStore is the blob side of the audit store.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// S3Config locates a bucket on an S3-compatible service.
type S3Config struct {
	Endpoint  string // empty = AWS
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// LoadS3Credentials reads files access-key and secret-key from dir.
func LoadS3Credentials(dir string) (access, secret string, err error) {
	if access, err = readTrimmed(filepath.Join(dir, "access-key")); err != nil {
		return "", "", err
	}
	if secret, err = readTrimmed(filepath.Join(dir, "secret-key")); err != nil {
		return "", "", err
	}
	return access, secret, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// S3 is an ObjectStore backed by one bucket.
type S3 struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	region   string
}

// NewS3 builds a client. Checksums are only sent when the API requires
// them, which keeps S3-compatible stores (Silo/MinIO, Ceph) happy.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("s3 config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3{
		client:   client,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) { u.PartSize = 8 << 20 }),
		bucket:   cfg.Bucket,
		region:   region,
	}, nil
}

// Put uploads body (multipart above 8 MiB).
func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// Get streams an object.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	return out.Body, nil
}

// EnsureBucket creates the bucket when it does not exist.
func (s *S3) EnsureBucket(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err == nil {
		return nil
	}
	in := &s3.CreateBucketInput{Bucket: aws.String(s.bucket)}
	if s.region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(s.region)}
	}
	_, err := s.client.CreateBucket(ctx, in)
	var owned *types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &owned) {
		return fmt.Errorf("s3 create bucket %s: %w", s.bucket, err)
	}
	return nil
}

// EnsureLifecycle expires raw/ and files/ objects after days.
func (s *S3) EnsureLifecycle(ctx context.Context, days int) error {
	rule := func(id, prefix string) types.LifecycleRule {
		return types.LifecycleRule{
			ID:         aws.String(id),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(prefix)},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(int32(days))},
		}
	}
	_, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(s.bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{
			rule("devpod-audit-raw", "raw/"),
			rule("devpod-audit-files", "files/"),
		}},
	})
	if err != nil {
		return fmt.Errorf("s3 lifecycle: %w", err)
	}
	return nil
}
