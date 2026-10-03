package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type s3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

func newS3(cfg Config, creds Credentials) (Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return nil, fmt.Errorf("S3 access key and secret key are required")
	}
	if cfg.Endpoint != "" {
		endpoint, err := url.Parse(cfg.Endpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, fmt.Errorf("invalid S3 endpoint")
		}
	}
	prefix, err := normalizePrefix(cfg.Prefix)
	if err != nil {
		return nil, fmt.Errorf("invalid S3 prefix: %w", err)
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	provider := credentials.NewStaticCredentialsProvider(creds.AccessKey, creds.SecretKey, creds.SessionToken)
	httpClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(provider),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("configure S3 client")
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimSuffix(cfg.Endpoint, "/"))
		}
		options.UsePathStyle = cfg.PathStyle
		// A conditional write must never be replayed after an ambiguous response.
		// Disabling SDK retries for this client keeps all mutations to one attempt.
		options.Retryer = aws.NopRetryer{}
	})
	return &s3Store{client: client, bucket: cfg.Bucket, prefix: prefix}, nil
}

func (s *s3Store) actualKey(key string) string { return joinKey(s.prefix, key) }

func (s *s3Store) List(ctx context.Context, key string) ([]Entry, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return nil, err
	}
	actualPrefix := s.actualKey(key)
	if actualPrefix != "" {
		actualPrefix += "/"
	}
	entries := make(map[string]Entry)
	var token *string
	seenTokens := make(map[string]struct{})
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(actualPrefix),
			Delimiter:         aws.String("/"),
			MaxKeys:           aws.Int32(1000),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, mapS3Error("list", err)
		}
		for _, object := range out.Contents {
			if object.Key == nil {
				return nil, fmt.Errorf("S3 listing returned an object without a key")
			}
			full := *object.Key
			if !strings.HasPrefix(full, actualPrefix) {
				continue
			}
			relative := strings.TrimPrefix(full, actualPrefix)
			if relative == "" {
				continue
			}
			first, _, nested := strings.Cut(relative, "/")
			if first == "" {
				return nil, fmt.Errorf("S3 listing returned an invalid key")
			}
			childPath := joinKey(key, first)
			if nested || strings.HasSuffix(relative, "/") {
				if err := addS3ListingEntry(entries, Entry{Path: childPath, Name: first, IsDir: true, Modified: derefTime(object.LastModified)}); err != nil {
					return nil, err
				}
				continue
			}
			e := Entry{Path: childPath, Name: first, Size: derefInt64(object.Size), Modified: derefTime(object.LastModified), ETag: aws.ToString(object.ETag)}
			if err := addS3ListingEntry(entries, e); err != nil {
				return nil, err
			}
		}
		for _, common := range out.CommonPrefixes {
			if common.Prefix == nil {
				return nil, fmt.Errorf("S3 listing returned a common prefix without a prefix")
			}
			full := *common.Prefix
			if !strings.HasPrefix(full, actualPrefix) {
				continue
			}
			relative := strings.TrimSuffix(strings.TrimPrefix(full, actualPrefix), "/")
			if relative == "" {
				continue
			}
			first, _, _ := strings.Cut(relative, "/")
			childPath := joinKey(key, first)
			if err := addS3ListingEntry(entries, Entry{Path: childPath, Name: first, IsDir: true}); err != nil {
				return nil, err
			}
		}
		if len(entries) > maxListEntries {
			return nil, fmt.Errorf("S3 listing exceeds %d entries", maxListEntries)
		}
		if out.IsTruncated == nil {
			return nil, fmt.Errorf("incomplete S3 listing: response omitted IsTruncated")
		}
		if !*out.IsTruncated {
			break
		}
		next := aws.ToString(out.NextContinuationToken)
		if next == "" {
			return nil, fmt.Errorf("incomplete S3 listing: truncated page has no continuation token")
		}
		if _, exists := seenTokens[next]; exists || token != nil && next == *token {
			return nil, fmt.Errorf("incomplete S3 listing: repeated continuation token")
		}
		seenTokens[next] = struct{}{}
		token = aws.String(next)
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func addS3ListingEntry(entries map[string]Entry, entry Entry) error {
	previous, exists := entries[entry.Path]
	if !exists {
		entries[entry.Path] = entry
		return nil
	}
	if previous.IsDir != entry.IsDir {
		return fmt.Errorf("S3 listing conflict: %q is both a file and a directory", entry.Path)
	}
	if !entry.IsDir && (previous.ETag != entry.ETag || previous.Size != entry.Size) {
		return fmt.Errorf("S3 listing conflict: %q changed while listing", entry.Path)
	}
	if previous.Modified.IsZero() && !entry.Modified.IsZero() {
		previous.Modified = entry.Modified
		entries[entry.Path] = previous
	}
	return nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (Entry, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return Entry{}, err
	}
	if key == "" {
		return Entry{Path: "", Name: "", IsDir: true}, nil
	}
	full := s.actualKey(key)
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(full)})
	if err == nil {
		return s.entryFromHead(key, out), nil
	}
	if !isS3NotFound(err) {
		return Entry{}, mapS3Error("stat", err)
	}
	markerKey := full + "/"
	marker, markerErr := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(markerKey)})
	if markerErr == nil {
		e := s.entryFromHead(key, marker)
		e.Path, e.Name, e.IsDir, e.Size = key, path.Base(key), true, 0
		return e, nil
	}
	if !isS3NotFound(markerErr) {
		return Entry{}, mapS3Error("stat directory marker", markerErr)
	}
	list, listErr := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(markerKey),
		MaxKeys: aws.Int32(1),
	})
	if listErr != nil {
		return Entry{}, mapS3Error("stat directory", listErr)
	}
	if len(list.Contents) == 0 && len(list.CommonPrefixes) == 0 {
		return Entry{}, ErrNotFound
	}
	return Entry{Path: key, Name: path.Base(key), IsDir: true}, nil
}

func (s *s3Store) entryFromHead(key string, out *s3.HeadObjectOutput) Entry {
	return Entry{Path: key, Name: path.Base(key), Size: derefInt64(out.ContentLength), Modified: derefTime(out.LastModified), ETag: aws.ToString(out.ETag)}
}

func (s *s3Store) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key))}
	if etag != "" {
		input.IfMatch = aws.String(etag)
	}
	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, Entry{}, mapS3Error("open", err)
	}
	eTag := aws.ToString(out.ETag)
	if etag != "" && eTag != etag {
		_ = out.Body.Close()
		return nil, Entry{}, ErrConflict
	}
	e := Entry{Path: key, Name: path.Base(key), Size: derefInt64(out.ContentLength), Modified: derefTime(out.LastModified), ETag: eTag}
	return out.Body, e, nil
}

func (s *s3Store) OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	if etag == "" || strings.HasPrefix(strings.TrimSpace(etag), "W/") {
		return nil, Entry{}, ErrUnsupported
	}
	if start < 0 || length <= 0 || start > int64(^uint64(0)>>1)-length {
		return nil, Entry{}, ErrInvalidRange
	}
	end := start + length - 1
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:  aws.String(s.bucket),
		Key:     aws.String(s.actualKey(key)),
		IfMatch: aws.String(etag),
		Range:   aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	})
	if err != nil {
		return nil, Entry{}, mapS3Error("range read", err)
	}
	if out.ETag == nil || aws.ToString(out.ETag) != etag || derefInt64(out.ContentLength) != length ||
		out.ContentRange == nil || !strings.HasPrefix(aws.ToString(out.ContentRange), fmt.Sprintf("bytes %d-%d/", start, end)) {
		_ = out.Body.Close()
		return nil, Entry{}, ErrConflict
	}
	contentRange := aws.ToString(out.ContentRange)
	if !validContentRangeTotal(contentRange, start, end) {
		_ = out.Body.Close()
		return nil, Entry{}, ErrConflict
	}
	e := Entry{Path: key, Name: path.Base(key), Size: derefInt64(out.ContentLength), Modified: derefTime(out.LastModified), ETag: aws.ToString(out.ETag)}
	return out.Body, e, nil
}

func validContentRangeTotal(value string, start, end int64) bool {
	prefix := fmt.Sprintf("bytes %d-%d/", start, end)
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(value, prefix), 10, 64)
	return err == nil && total > end
}

func (s *s3Store) ListVersions(ctx context.Context, key string) ([]ObjectVersion, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, err
	}
	actualKey := s.actualKey(key)
	var keyMarker, versionMarker *string
	seenMarkers := make(map[string]struct{})
	versions := make([]ObjectVersion, 0)
	for {
		out, listErr := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket:          aws.String(s.bucket),
			Prefix:          aws.String(actualKey),
			KeyMarker:       keyMarker,
			VersionIdMarker: versionMarker,
			MaxKeys:         aws.Int32(1000),
		})
		if listErr != nil {
			return nil, mapS3Error("list versions", listErr)
		}
		for _, item := range out.Versions {
			if aws.ToString(item.Key) != actualKey {
				continue
			}
			versionID := aws.ToString(item.VersionId)
			if versionID == "" {
				return nil, fmt.Errorf("incomplete S3 version listing: version has no ID")
			}
			entry := Entry{Path: key, Name: path.Base(key), Size: derefInt64(item.Size), Modified: derefTime(item.LastModified), ETag: aws.ToString(item.ETag), VersionID: versionID}
			versions = append(versions, ObjectVersion{Entry: entry, VersionID: versionID, IsLatest: aws.ToBool(item.IsLatest)})
		}
		for _, marker := range out.DeleteMarkers {
			if aws.ToString(marker.Key) != actualKey {
				continue
			}
			versionID := aws.ToString(marker.VersionId)
			if versionID == "" {
				return nil, fmt.Errorf("incomplete S3 version listing: delete marker has no ID")
			}
			entry := Entry{Path: key, Name: path.Base(key), Modified: derefTime(marker.LastModified), VersionID: versionID}
			versions = append(versions, ObjectVersion{Entry: entry, VersionID: versionID, IsLatest: aws.ToBool(marker.IsLatest), DeleteMarker: true})
		}
		if len(versions) > maxListEntries {
			return nil, fmt.Errorf("S3 version listing exceeds %d entries", maxListEntries)
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		nextKey, nextVersion := aws.ToString(out.NextKeyMarker), aws.ToString(out.NextVersionIdMarker)
		if nextKey == "" || nextVersion == "" {
			return nil, fmt.Errorf("incomplete S3 version listing: truncated page has no continuation markers")
		}
		marker := nextKey + "\x00" + nextVersion
		if _, seen := seenMarkers[marker]; seen || keyMarker != nil && nextKey == *keyMarker && versionMarker != nil && nextVersion == *versionMarker {
			return nil, fmt.Errorf("incomplete S3 version listing: repeated continuation markers")
		}
		seenMarkers[marker] = struct{}{}
		keyMarker, versionMarker = aws.String(nextKey), aws.String(nextVersion)
	}
	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].Entry.Modified.After(versions[j].Entry.Modified)
	})
	return versions, nil
}

func (s *s3Store) OpenVersion(ctx context.Context, key, versionID string) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	if versionID == "" {
		return nil, Entry{}, ErrUnsupported
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key)), VersionId: aws.String(versionID)})
	if err != nil {
		return nil, Entry{}, mapS3Error("open version", err)
	}
	returnedVersion := aws.ToString(out.VersionId)
	if returnedVersion != "" && returnedVersion != versionID {
		_ = out.Body.Close()
		return nil, Entry{}, ErrConflict
	}
	e := Entry{Path: key, Name: path.Base(key), Size: derefInt64(out.ContentLength), Modified: derefTime(out.LastModified), ETag: aws.ToString(out.ETag), VersionID: versionID}
	return out.Body, e, nil
}

func (s *s3Store) CreateMultipart(ctx context.Context, key string) (string, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return "", err
	}
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key))})
	if err != nil {
		return "", mapS3Error("create multipart upload", err)
	}
	if out.UploadId == nil || aws.ToString(out.UploadId) == "" {
		return "", fmt.Errorf("S3 did not return a multipart upload ID")
	}
	return aws.ToString(out.UploadId), nil
}

func (s *s3Store) UploadPart(ctx context.Context, key, uploadID string, partNumber int32, body io.Reader, size int64) (MultipartPart, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return MultipartPart{}, err
	}
	if uploadID == "" || partNumber < 1 || partNumber > 10000 || body == nil || size < 0 {
		return MultipartPart{}, fmt.Errorf("invalid multipart upload part")
	}
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key)), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(partNumber), Body: body, ContentLength: aws.Int64(size),
	})
	if err != nil {
		return MultipartPart{}, mapS3Error("upload part", err)
	}
	if out.ETag == nil || aws.ToString(out.ETag) == "" {
		return MultipartPart{}, fmt.Errorf("S3 did not return a multipart part ETag")
	}
	return MultipartPart{Number: partNumber, ETag: aws.ToString(out.ETag), Size: size}, nil
}

func (s *s3Store) ListParts(ctx context.Context, key, uploadID string) ([]MultipartPart, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, err
	}
	if uploadID == "" {
		return nil, fmt.Errorf("multipart upload ID is required")
	}
	var marker *string
	seen := make(map[string]struct{})
	parts := make([]MultipartPart, 0)
	for {
		out, listErr := s.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key)), UploadId: aws.String(uploadID), PartNumberMarker: marker, MaxParts: aws.Int32(1000)})
		if listErr != nil {
			return nil, mapS3Error("list multipart parts", listErr)
		}
		for _, item := range out.Parts {
			if item.PartNumber == nil || item.ETag == nil || aws.ToString(item.ETag) == "" || aws.ToInt32(item.PartNumber) < 1 || aws.ToInt32(item.PartNumber) > 10000 {
				return nil, fmt.Errorf("incomplete S3 multipart listing: invalid part receipt")
			}
			parts = append(parts, MultipartPart{Number: aws.ToInt32(item.PartNumber), ETag: aws.ToString(item.ETag), Size: derefInt64(item.Size)})
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		next := aws.ToString(out.NextPartNumberMarker)
		if next == "" || marker != nil && next == *marker {
			return nil, fmt.Errorf("incomplete S3 multipart listing: repeated or missing continuation marker")
		}
		if _, exists := seen[next]; exists {
			return nil, fmt.Errorf("incomplete S3 multipart listing: repeated continuation marker")
		}
		seen[next] = struct{}{}
		marker = aws.String(next)
	}
	if len(parts) > 10000 {
		return nil, fmt.Errorf("S3 multipart listing exceeds the 10000-part limit")
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	for i := 1; i < len(parts); i++ {
		if parts[i-1].Number == parts[i].Number {
			return nil, fmt.Errorf("incomplete S3 multipart listing: duplicate part number")
		}
	}
	return parts, nil
}

func (s *s3Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart, condition Condition) (Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return Entry{}, err
	}
	if uploadID == "" || len(parts) == 0 || len(parts) > 10000 {
		return Entry{}, fmt.Errorf("invalid multipart completion request")
	}
	if err := ensureNoConditionConflict(condition); err != nil {
		return Entry{}, err
	}
	ordered := append([]MultipartPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Number < ordered[j].Number })
	completed := make([]types.CompletedPart, 0, len(ordered))
	var size int64
	for i, part := range ordered {
		if part.Number < 1 || part.Number > 10000 || part.ETag == "" || part.Size < 0 || i > 0 && ordered[i-1].Number == part.Number {
			return Entry{}, fmt.Errorf("invalid multipart part receipt")
		}
		if size > int64(^uint64(0)>>1)-part.Size {
			return Entry{}, fmt.Errorf("multipart object size overflows int64")
		}
		size += part.Size
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(part.Number), ETag: aws.String(part.ETag)})
	}
	input := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key)), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}
	if condition.IfMatch != "" {
		input.IfMatch = aws.String(condition.IfMatch)
	}
	if condition.IfNoneMatch {
		input.IfNoneMatch = aws.String("*")
	}
	out, err := s.client.CompleteMultipartUpload(ctx, input)
	if err != nil {
		return Entry{}, mapS3Error("complete multipart upload", err)
	}
	return Entry{Path: key, Name: path.Base(key), Size: size, Modified: time.Now().UTC(), ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId)}, nil
}

func (s *s3Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	key, err := normalizeKey(key, false)
	if err != nil {
		return err
	}
	if uploadID == "" {
		return fmt.Errorf("multipart upload ID is required")
	}
	_, err = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key)), UploadId: aws.String(uploadID)})
	if err != nil {
		return mapS3Error("abort multipart upload", err)
	}
	return nil
}

func (s *s3Store) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return Entry{}, err
	}
	if err := ensureNoConditionConflict(condition); err != nil {
		return Entry{}, err
	}
	if body == nil || size < 0 {
		return Entry{}, fmt.Errorf("invalid object body")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return Entry{}, err
	}
	input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key)), Body: body, ContentLength: aws.Int64(size)}
	if condition.IfMatch != "" {
		input.IfMatch = aws.String(condition.IfMatch)
	}
	if condition.IfNoneMatch {
		input.IfNoneMatch = aws.String("*")
	}
	out, err := s.client.PutObject(ctx, input)
	if err != nil {
		return Entry{}, mapS3Error("put", err)
	}
	e := Entry{Path: key, Name: path.Base(key), Size: size, Modified: time.Now().UTC(), ETag: aws.ToString(out.ETag)}
	return e, nil
}

func (s *s3Store) Delete(ctx context.Context, key string, condition Condition) error {
	key, err := normalizeKey(key, false)
	if err != nil {
		return err
	}
	if err := ensureNoConditionConflict(condition); err != nil {
		return err
	}
	if condition.IfNoneMatch {
		return ErrUnsupported
	}
	input := &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.actualKey(key))}
	if condition.IfMatch != "" {
		input.IfMatch = aws.String(condition.IfMatch)
	}
	_, err = s.client.DeleteObject(ctx, input)
	if err != nil {
		return mapS3Error("delete", err)
	}
	return nil
}

func (s *s3Store) Mkdir(ctx context.Context, key string) error {
	key, err := normalizeKey(key, true)
	if err != nil {
		return err
	}
	if key == "" {
		return nil
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.actualKey(key) + "/"),
		Body:          strings.NewReader(""),
		ContentLength: aws.Int64(0),
		IfNoneMatch:   aws.String("*"),
	})
	if err != nil {
		return mapS3Error("mkdir", err)
	}
	return nil
}

func mapS3Error(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		switch responseError.HTTPStatusCode() {
		case 416:
			return fmt.Errorf("%s: %w", op, ErrInvalidRange)
		case 501:
			return fmt.Errorf("%s: %w", op, ErrUnsupported)
		case 404:
			return fmt.Errorf("%s: %w", op, ErrNotFound)
		case 409, 412:
			return fmt.Errorf("%s: %w", op, ErrConflict)
		}
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch strings.ToLower(apiError.ErrorCode()) {
		case "notimplemented", "unsupportedoperation":
			return fmt.Errorf("%s: %w", op, ErrUnsupported)
		case "nosuchkey", "notfound", "no_such_key":
			return fmt.Errorf("%s: %w", op, ErrNotFound)
		case "preconditionfailed", "conditionalrequestconflict":
			return fmt.Errorf("%s: %w", op, ErrConflict)
		}
	}
	// SDK errors can contain response bodies echoed by compatible S3 services;
	// avoid returning them where a server could reflect submitted credentials.
	return fmt.Errorf("S3 %s failed", op)
}

func isS3NotFound(err error) bool {
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) && responseError.HTTPStatusCode() == 404 {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch strings.ToLower(apiError.ErrorCode()) {
		case "nosuchkey", "notfound", "no_such_key":
			return true
		}
	}
	return false
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

var _ Store = (*s3Store)(nil)
