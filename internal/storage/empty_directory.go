package storage

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// EmptyDirectoryStore can remove only an empty collection or its zero-byte
// marker. Implementations MUST NOT recursively remove children, even when a
// different client creates them concurrently. Generic WebDAV has no such primitive.
type EmptyDirectoryStore interface {
	RemoveEmptyDirectory(context.Context, string) error
}

func (m *memoryStore) RemoveEmptyDirectory(ctx context.Context, key string) error {
	return m.Delete(ctx, key, Condition{}) // The memory store checks and removes under one mutex.
}
func (s *s3Store) RemoveEmptyDirectory(ctx context.Context, key string) error {
	key, err := normalizeKey(key, false)
	if err != nil {
		return err
	}
	marker := s.actualKey(key) + "/"
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(marker)})
	if err != nil {
		mapped := mapS3Error("read directory marker", err)
		if errors.Is(mapped, ErrNotFound) {
			return nil
		}
		return mapped
	}
	if aws.ToInt64(head.ContentLength) != 0 || head.ETag == nil || !isStrongProbeETag(aws.ToString(head.ETag)) {
		return ErrConflict
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(marker), IfMatch: head.ETag})
	if err != nil {
		return mapS3Error("remove directory marker", err)
	}
	return nil
}
