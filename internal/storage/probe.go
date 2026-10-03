package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const multipartProbePartSize = 5 << 20

// Probe verifies the conditional operations needed for safe editing. It uses
// one cryptographically random, isolated key and removes it only with a known
// ETag so a failed capability check cannot delete an unrelated object.
func Probe(ctx context.Context, store Store) (caps Capabilities, resultErr error) {
	if store == nil {
		return Capabilities{}, fmt.Errorf("storage is nil")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Capabilities{}, fmt.Errorf("create storage probe key: %w", err)
	}
	key := ".tamiops-probe-" + hex.EncodeToString(token[:])
	created := false
	currentETag := ""
	defer func() {
		if !created {
			return
		}
		if currentETag == "" {
			resultErr = errors.Join(resultErr, fmt.Errorf("temporary probe cleanup failed for key %q: no ETag is available for a safe delete", key))
			caps = Capabilities{}
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Cleanup stays conditional because the temporary key may have changed.
		if err := store.Delete(cleanupCtx, key, Condition{IfMatch: currentETag}); err != nil && !errors.Is(err, ErrNotFound) {
			resultErr = errors.Join(resultErr, fmt.Errorf("temporary probe cleanup failed for key %q", key))
			caps = Capabilities{}
		}
	}()

	initialBytes := []byte("storage-probe-v1")
	initial, err := store.Put(ctx, key, bytes.NewReader(initialBytes), int64(len(initialBytes)), Condition{IfNoneMatch: true})
	if err != nil {
		return Capabilities{}, fmt.Errorf("conditional create probe failed: %w", err)
	}
	created = true
	currentETag = initial.ETag
	if !isStrongProbeETag(currentETag) {
		return Capabilities{}, fmt.Errorf("conditional create probe returned no strong ETag for key %q", key)
	}

	verifyUnchanged := func(expected []byte) error {
		if err := verifyProbeContent(ctx, store, key, currentETag, expected); err != nil {
			if latest, statErr := store.Stat(ctx, key); statErr == nil && latest.ETag != "" {
				currentETag = latest.ETag
			}
			return err
		}
		latest, err := store.Stat(ctx, key)
		if err != nil {
			return fmt.Errorf("stat after conditional probe failed")
		}
		if latest.ETag != currentETag {
			currentETag = latest.ETag
			return fmt.Errorf("conditional rejection changed the object ETag")
		}
		return nil
	}

	_, createErr := store.Put(ctx, key, bytes.NewReader([]byte("duplicate")), int64(len("duplicate")), Condition{IfNoneMatch: true})
	contentErr := verifyUnchanged(initialBytes)
	if contentErr != nil {
		return Capabilities{}, fmt.Errorf("conditional create did not preserve probe contents: %w", contentErr)
	}
	if !errors.Is(createErr, ErrConflict) {
		if createErr == nil {
			return Capabilities{}, fmt.Errorf("conditional create probe was not rejected")
		}
		return Capabilities{}, fmt.Errorf("conditional create probe returned an unexpected error")
	}

	wrongETag := "\"probe-invalid-" + hex.EncodeToString(token[:]) + "\""
	_, overwriteErr := store.Put(ctx, key, bytes.NewReader([]byte("wrong-etag")), int64(len("wrong-etag")), Condition{IfMatch: wrongETag})
	contentErr = verifyUnchanged(initialBytes)
	if contentErr != nil {
		return Capabilities{}, fmt.Errorf("wrong ETag write did not preserve probe contents: %w", contentErr)
	}
	if !errors.Is(overwriteErr, ErrConflict) {
		if overwriteErr == nil {
			return Capabilities{}, fmt.Errorf("wrong ETag conditional write was not rejected")
		}
		return Capabilities{}, fmt.Errorf("wrong ETag conditional write returned an unexpected error")
	}

	updatedBytes := []byte("storage-probe-v2")
	updated, err := store.Put(ctx, key, bytes.NewReader(updatedBytes), int64(len(updatedBytes)), Condition{IfMatch: currentETag})
	if err != nil {
		return Capabilities{}, fmt.Errorf("correct ETag conditional write failed: %w", err)
	}
	currentETag = updated.ETag
	if !isStrongProbeETag(currentETag) {
		return Capabilities{}, fmt.Errorf("conditional write probe returned no strong ETag for key %q", key)
	}
	if currentETag == initial.ETag {
		return Capabilities{}, fmt.Errorf("conditional write probe did not produce a new ETag for key %q", key)
	}

	deleteErr := store.Delete(ctx, key, Condition{IfMatch: wrongETag})
	contentErr = verifyUnchanged(updatedBytes)
	if contentErr != nil {
		return Capabilities{}, fmt.Errorf("wrong ETag delete did not preserve probe contents: %w", contentErr)
	}
	if !errors.Is(deleteErr, ErrConflict) {
		if deleteErr == nil {
			return Capabilities{}, fmt.Errorf("wrong ETag conditional delete was not rejected")
		}
		return Capabilities{}, fmt.Errorf("wrong ETag conditional delete returned an unexpected error")
	}

	caps = Capabilities{
		ConditionalWrite:  true,
		ConditionalDelete: true,
		RangeRead:         probeRangeRead(ctx, store, key, currentETag, updatedBytes),
	}
	if multipart, ok := store.(MultipartStore); ok {
		// This is independent from ordinary conditional PUT/DELETE support. Core
		// only enables resumable large uploads when the final multipart commit
		// enforces both overwrite and create-only conditions.
		var latestETag string
		latestETag, caps.MultipartConditional, resultErr = probeMultipartConditional(ctx, store, multipart, key, token)
		if latestETag != "" {
			currentETag = latestETag
		}
		if resultErr != nil {
			return caps, resultErr
		}
	}
	if err := store.Delete(ctx, key, Condition{IfMatch: currentETag}); err != nil {
		return Capabilities{}, fmt.Errorf("correct ETag conditional delete failed: %w", err)
	}
	created = false
	return caps, nil
}

func probeRangeRead(ctx context.Context, store Store, key, etag string, expected []byte) bool {
	ranged, ok := store.(RangeStore)
	if !ok || len(expected) == 0 || etag == "" || len(etag) >= 2 && strings.HasPrefix(strings.TrimSpace(etag), "W/") {
		return false
	}
	body, entry, err := ranged.OpenRange(ctx, key, etag, 0, 1)
	if err != nil || body == nil {
		return false
	}
	read, readErr := io.ReadAll(io.LimitReader(body, 2))
	closeErr := body.Close()
	return readErr == nil && closeErr == nil && len(read) == 1 && read[0] == expected[0] && entry.ETag == etag && entry.Size == 1 && !entry.IsDir
}

func isStrongProbeETag(etag string) bool {
	etag = strings.TrimSpace(etag)
	return etag != "" && !strings.HasPrefix(strings.ToUpper(etag), "W/")
}

func probeMultipartConditional(ctx context.Context, store Store, multipart MultipartStore, key string, nonce [16]byte) (currentETag string, supported bool, resultErr error) {
	payload := bytes.Repeat([]byte("m"), multipartProbePartSize)
	oldContent := []byte("storage-probe-v2")
	current, err := store.Stat(ctx, key)
	if err != nil || current.ETag == "" {
		return "", false, nil
	}
	currentETag = current.ETag
	verify := func(expected []byte) bool {
		if err := verifyProbeContent(ctx, store, key, currentETag, expected); err != nil {
			if latest, statErr := store.Stat(ctx, key); statErr == nil && latest.ETag != "" {
				currentETag = latest.ETag
			}
			return false
		}
		latest, err := store.Stat(ctx, key)
		if err != nil || latest.ETag != currentETag {
			if err == nil && latest.ETag != "" {
				currentETag = latest.ETag
			}
			return false
		}
		return true
	}

	newKey := key + "-multipart-create"
	if _, err := store.Stat(ctx, newKey); err == nil || !errors.Is(err, ErrNotFound) {
		return currentETag, false, nil
	}
	newKeyCreated := false
	newKeyETag := ""
	defer func() {
		if !newKeyCreated {
			return
		}
		if newKeyETag == "" {
			resultErr = errors.Join(resultErr, fmt.Errorf("multipart probe cleanup failed for key %q: no ETag is available for a safe delete", newKey))
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.Delete(cleanupCtx, newKey, Condition{IfMatch: newKeyETag}); err != nil && !errors.Is(err, ErrNotFound) {
			resultErr = errors.Join(resultErr, fmt.Errorf("multipart probe cleanup failed for key %q", newKey))
		}
	}()

	createAndUpload := func(target string) (string, []MultipartPart, func(), error) {
		uploadID, err := multipart.CreateMultipart(ctx, target)
		if err != nil || uploadID == "" {
			return "", nil, nil, err
		}
		abort := func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = multipart.AbortMultipart(cleanupCtx, target, uploadID)
		}
		part, err := multipart.UploadPart(ctx, target, uploadID, 1, bytes.NewReader(payload), int64(len(payload)))
		if err != nil || part.Number != 1 || part.ETag == "" || part.Size != int64(len(payload)) {
			return uploadID, nil, abort, err
		}
		remoteParts, err := multipart.ListParts(ctx, target, uploadID)
		if err != nil || len(remoteParts) != 1 || remoteParts[0] != part {
			return uploadID, nil, abort, err
		}
		return uploadID, []MultipartPart{part}, abort, nil
	}

	// Prove stale If-Match and create-only If-None-Match are independently
	// enforced against an existing destination, with no mutation on rejection.
	wrongUploadID, wrongParts, abortWrong, err := createAndUpload(key)
	if err != nil || wrongUploadID == "" {
		if abortWrong != nil {
			abortWrong()
		}
		return currentETag, false, nil
	}
	defer abortWrong()
	wrongTag := `"probe-invalid-` + hex.EncodeToString(nonce[:]) + `"`
	_, wrongMatchErr := multipart.CompleteMultipart(ctx, key, wrongUploadID, wrongParts, Condition{IfMatch: wrongTag})
	if !errors.Is(wrongMatchErr, ErrConflict) || !verify(oldContent) {
		if wrongMatchErr == nil {
			if latest, statErr := store.Stat(ctx, key); statErr == nil && latest.ETag != "" {
				currentETag = latest.ETag
			}
		}
		return currentETag, false, nil
	}
	_, createOnlyErr := multipart.CompleteMultipart(ctx, key, wrongUploadID, wrongParts, Condition{IfNoneMatch: true})
	if !errors.Is(createOnlyErr, ErrConflict) || !verify(oldContent) {
		if createOnlyErr == nil {
			if latest, statErr := store.Stat(ctx, key); statErr == nil && latest.ETag != "" {
				currentETag = latest.ETag
			}
		}
		return currentETag, false, nil
	}
	previousETag := currentETag
	completed, err := multipart.CompleteMultipart(ctx, key, wrongUploadID, wrongParts, Condition{IfMatch: currentETag})
	if err != nil {
		if latest, statErr := store.Stat(ctx, key); statErr == nil && latest.ETag != "" {
			currentETag = latest.ETag
		}
		return currentETag, false, nil
	}
	if completed.ETag != "" {
		currentETag = completed.ETag
	}
	if !isStrongProbeETag(currentETag) {
		latest, statErr := store.Stat(ctx, key)
		if statErr != nil || !isStrongProbeETag(latest.ETag) {
			return currentETag, false, nil
		}
		currentETag = latest.ETag
	}
	if verifyProbeContent(ctx, store, key, currentETag, payload) != nil {
		return currentETag, false, nil
	}
	if currentETag == previousETag {
		return currentETag, false, nil
	}

	// If-None-Match is also exercised on a genuinely absent destination, which is
	// the condition Core uses when creating a new large object.
	newUploadID, newParts, abortNew, err := createAndUpload(newKey)
	if err != nil || newUploadID == "" {
		if abortNew != nil {
			abortNew()
		}
		return currentETag, false, nil
	}
	defer abortNew()
	newEntry, err := multipart.CompleteMultipart(ctx, newKey, newUploadID, newParts, Condition{IfNoneMatch: true})
	if err != nil {
		return currentETag, false, nil
	}
	if newEntry.ETag != "" {
		newKeyETag = newEntry.ETag
	}
	newKeyCreated = true
	if newKeyETag == "" {
		latest, statErr := store.Stat(ctx, newKey)
		if statErr != nil || latest.ETag == "" {
			return currentETag, false, nil
		}
		newKeyETag = latest.ETag
	}
	newStat, err := store.Stat(ctx, newKey)
	if err != nil || newStat.Size != int64(len(payload)) || newStat.ETag != newKeyETag {
		return currentETag, false, nil
	}
	if verifyProbeContent(ctx, store, newKey, newKeyETag, payload) != nil {
		return currentETag, false, nil
	}
	return currentETag, true, nil
}

func verifyProbeContent(ctx context.Context, store Store, key, etag string, expected []byte) error {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, entry, err := store.Open(readCtx, key, etag)
	if err != nil {
		return fmt.Errorf("open conditional probe object failed")
	}
	read, readErr := io.ReadAll(io.LimitReader(body, int64(len(expected)+1)))
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("read conditional probe object failed")
	}
	if entry.ETag != "" && entry.ETag != etag {
		return fmt.Errorf("conditional probe read returned a different ETag")
	}
	if len(read) != len(expected) {
		return fmt.Errorf("conditional probe content length mismatch")
	}
	if !bytes.Equal(read, expected) {
		return fmt.Errorf("conditional probe content mismatch")
	}
	return nil
}
