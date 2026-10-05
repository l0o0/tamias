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
// one cryptographically random, isolated key. Cleanup requires its nonce-bearing
// content and a known ETag; on a server that ignores conditions this remains
// best-effort cleanup of our private probe, never proof of safe write support.
func Probe(ctx context.Context, store Store) (caps Capabilities, resultErr error) {
	if store == nil {
		return Capabilities{}, fmt.Errorf("storage is nil")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Capabilities{}, fmt.Errorf("create storage probe key: %w", err)
	}
	probeID := hex.EncodeToString(token[:])
	key := ".tamiops-probe-" + probeID
	created := false
	currentETag := ""
	var currentContent []byte
	defer func() {
		if !created {
			return
		}
		if currentETag == "" {
			resultErr = errors.Join(resultErr, fmt.Errorf("临时验证文件 %q 无法确认版本，已保留以避免误删", key))
			caps = Capabilities{}
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// A failed condition check may have changed the object, even without
		// advancing its ETag. Never clean up content we cannot still identify.
		if err := verifyProbeContent(cleanupCtx, store, key, currentETag, currentContent); err != nil {
			if !errors.Is(err, ErrNotFound) {
				resultErr = errors.Join(resultErr, fmt.Errorf("临时验证文件 %q 的内容或版本已变化，已保留以避免误删", key))
				caps = Capabilities{}
			}
			return
		}
		// Cleanup stays conditional because the temporary key may have changed.
		if err := store.Delete(cleanupCtx, key, Condition{IfMatch: currentETag}); err != nil && !errors.Is(err, ErrNotFound) {
			resultErr = errors.Join(resultErr, fmt.Errorf("临时验证文件 %q 清理失败：%w", key, err))
			caps = Capabilities{}
		}
	}()

	// A per-probe payload lets a read-back distinguish our write from an
	// unrelated revision when PUT does not include a response validator.
	initialBytes := []byte("storage-probe-v1:" + probeID)
	initial, err := store.Put(ctx, key, bytes.NewReader(initialBytes), int64(len(initialBytes)), Condition{IfNoneMatch: true})
	if err != nil {
		return Capabilities{}, fmt.Errorf("conditional create probe failed: %w", err)
	}
	created = true
	currentETag, err = probeWriteETag(ctx, store, key, initial.ETag, initialBytes)
	if err != nil {
		return Capabilities{}, fmt.Errorf("conditional create probe could not confirm the version for key %q: %w", key, err)
	}
	initialETag := currentETag
	currentContent = initialBytes

	// A server that ignores a condition can accept the negative test write.
	// Recover only our unique payload for cleanup, never an arbitrary Stat tag.
	recoverProbeWrite := func(entry Entry, payload []byte) {
		currentETag, _ = probeWriteETag(ctx, store, key, entry.ETag, payload)
		currentContent = payload
	}

	verifyUnchanged := func(expected []byte) error {
		if err := verifyProbeContent(ctx, store, key, currentETag, expected); err != nil {
			return err
		}
		latest, err := store.Stat(ctx, key)
		if err != nil {
			return fmt.Errorf("stat after conditional probe failed")
		}
		if latest.ETag != currentETag {
			// Preserve the verified version for conditional cleanup. A newer
			// revision may belong to another writer and must not be adopted.
			return fmt.Errorf("conditional rejection changed the object ETag")
		}
		return nil
	}

	duplicateBytes := []byte("storage-probe-duplicate:" + probeID)
	duplicate, createErr := store.Put(ctx, key, bytes.NewReader(duplicateBytes), int64(len(duplicateBytes)), Condition{IfNoneMatch: true})
	if createErr == nil {
		recoverProbeWrite(duplicate, duplicateBytes)
		return Capabilities{}, fmt.Errorf("%w：服务器忽略了“仅新建、不覆盖”条件，安全写入与删除未启用", ErrConditionalUnsupported)
	}
	if !errors.Is(createErr, ErrConflict) {
		return Capabilities{}, fmt.Errorf("验证“仅新建”条件失败：%w", createErr)
	}
	contentErr := verifyUnchanged(initialBytes)
	if contentErr != nil {
		return Capabilities{}, fmt.Errorf("验证“仅新建”条件后，临时文件的内容或版本发生变化：%w", contentErr)
	}

	wrongETag := "\"probe-invalid-" + hex.EncodeToString(token[:]) + "\""
	wrongVersionBytes := []byte("storage-probe-wrong-version:" + probeID)
	overwrite, overwriteErr := store.Put(ctx, key, bytes.NewReader(wrongVersionBytes), int64(len(wrongVersionBytes)), Condition{IfMatch: wrongETag})
	if overwriteErr == nil {
		recoverProbeWrite(overwrite, wrongVersionBytes)
		return Capabilities{}, fmt.Errorf("%w：服务器接受了版本不匹配的覆盖请求，安全写入与删除未启用", ErrConditionalUnsupported)
	}
	if !errors.Is(overwriteErr, ErrConflict) {
		return Capabilities{}, fmt.Errorf("验证覆盖保护失败：%w", overwriteErr)
	}
	contentErr = verifyUnchanged(initialBytes)
	if contentErr != nil {
		return Capabilities{}, fmt.Errorf("验证覆盖保护后，临时文件的内容或版本发生变化：%w", contentErr)
	}

	updatedBytes := []byte("storage-probe-v2:" + probeID)
	updated, err := store.Put(ctx, key, bytes.NewReader(updatedBytes), int64(len(updatedBytes)), Condition{IfMatch: currentETag})
	if err != nil {
		return Capabilities{}, fmt.Errorf("correct ETag conditional write failed: %w", err)
	}
	currentETag, err = probeWriteETag(ctx, store, key, updated.ETag, updatedBytes)
	if err != nil {
		return Capabilities{}, fmt.Errorf("conditional write probe could not confirm the version for key %q: %w", key, err)
	}
	currentContent = updatedBytes
	if currentETag == initialETag {
		return Capabilities{}, fmt.Errorf("conditional write probe did not produce a new ETag for key %q", key)
	}

	deleteErr := store.Delete(ctx, key, Condition{IfMatch: wrongETag})
	if deleteErr == nil {
		return Capabilities{}, fmt.Errorf("%w：服务器接受了版本不匹配的删除请求，安全写入与删除未启用", ErrConditionalUnsupported)
	}
	if !errors.Is(deleteErr, ErrConflict) {
		return Capabilities{}, fmt.Errorf("验证删除保护失败：%w", deleteErr)
	}
	contentErr = verifyUnchanged(updatedBytes)
	if contentErr != nil {
		return Capabilities{}, fmt.Errorf("验证删除保护后，临时文件的内容或版本发生变化：%w", contentErr)
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
		latestETag, caps.MultipartConditional, resultErr = probeMultipartConditional(ctx, store, multipart, key, currentETag, token, updatedBytes)
		if latestETag != "" {
			if latestETag != currentETag {
				currentContent = multipartProbePayload(token)
			}
			currentETag = latestETag
		}
		if resultErr != nil {
			return Capabilities{}, resultErr
		}
	}
	if err := store.Delete(ctx, key, Condition{IfMatch: currentETag}); err != nil {
		return Capabilities{}, fmt.Errorf("correct ETag conditional delete failed: %w", err)
	}
	created = false
	return caps, nil
}

// probeWriteETag verifies the version written to an isolated probe object,
// resolving an omitted/weak PUT validator through Stat when needed. Bind a read
// to that strong ETag and verify the nonce-bearing content before using it.
// On failure, return no ETag so the caller cannot delete an unverified revision.
func probeWriteETag(ctx context.Context, store Store, key, responseETag string, expected []byte) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	etag := responseETag
	if !isStrongProbeETag(etag) {
		entry, err := store.Stat(readCtx, key)
		if err != nil {
			return "", fmt.Errorf("read probe properties after PUT: %w", err)
		}
		if !isStrongProbeETag(entry.ETag) {
			return "", fmt.Errorf("probe properties returned no strong ETag")
		}
		if entry.IsDir || entry.Size != int64(len(expected)) {
			return "", fmt.Errorf("probe properties do not match the uploaded file")
		}
		etag = entry.ETag
	}
	if err := verifyProbeContent(readCtx, store, key, etag, expected); err != nil {
		return "", fmt.Errorf("verify probe content after PUT: %w", err)
	}
	return etag, nil
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

func multipartProbePayload(nonce [16]byte) []byte {
	payload := bytes.Repeat([]byte("m"), multipartProbePartSize)
	copy(payload, "storage-multipart-probe:"+hex.EncodeToString(nonce[:]))
	return payload
}

func probeMultipartConditional(ctx context.Context, store Store, multipart MultipartStore, key, knownETag string, nonce [16]byte, oldContent []byte) (currentETag string, supported bool, resultErr error) {
	payload := multipartProbePayload(nonce)
	currentETag = knownETag
	verify := func(expected []byte) bool {
		if err := verifyProbeContent(ctx, store, key, currentETag, expected); err != nil {
			return false
		}
		latest, err := store.Stat(ctx, key)
		return err == nil && latest.ETag == currentETag
	}
	if !verify(oldContent) {
		return currentETag, false, nil
	}
	// An unsupported/ambiguous completion may have written our payload. Only
	// adopt that revision for cleanup after verifying it; never adopt a
	// foreign revision merely because Stat returned a newer tag.
	recoverWrittenProbe := func() {
		if etag, err := probeWriteETag(ctx, store, key, "", payload); err == nil {
			currentETag = etag
		}
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
		if err := verifyProbeContent(cleanupCtx, store, newKey, newKeyETag, payload); err != nil {
			if !errors.Is(err, ErrNotFound) {
				resultErr = errors.Join(resultErr, fmt.Errorf("分片验证文件 %q 的内容或版本已变化，已保留以避免误删", newKey))
			}
			return
		}
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
		recoverWrittenProbe()
		return currentETag, false, nil
	}
	_, createOnlyErr := multipart.CompleteMultipart(ctx, key, wrongUploadID, wrongParts, Condition{IfNoneMatch: true})
	if !errors.Is(createOnlyErr, ErrConflict) || !verify(oldContent) {
		recoverWrittenProbe()
		return currentETag, false, nil
	}
	previousETag := currentETag
	completed, err := multipart.CompleteMultipart(ctx, key, wrongUploadID, wrongParts, Condition{IfMatch: currentETag})
	if err != nil {
		recoverWrittenProbe()
		return currentETag, false, nil
	}
	verifiedETag, err := probeWriteETag(ctx, store, key, completed.ETag, payload)
	if err != nil {
		return currentETag, false, nil
	}
	currentETag = verifiedETag
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
		// A completion response can be lost after the object was committed.
		// Clean up only if the object still contains this probe's payload.
		verifiedETag, verifyErr := probeWriteETag(ctx, store, newKey, "", payload)
		if verifyErr == nil {
			newKeyCreated = true
			newKeyETag = verifiedETag
		} else if _, statErr := store.Stat(ctx, newKey); !errors.Is(statErr, ErrNotFound) {
			return currentETag, false, fmt.Errorf("multipart create probe could not confirm cleanup for key %q: %w", newKey, errors.Join(err, verifyErr))
		}
		return currentETag, false, nil
	}
	newKeyCreated = true
	newKeyETag, err = probeWriteETag(ctx, store, newKey, newEntry.ETag, payload)
	if err != nil {
		return currentETag, false, nil
	}
	return currentETag, true, nil
}

func verifyProbeContent(ctx context.Context, store Store, key, etag string, expected []byte) error {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, entry, err := store.Open(readCtx, key, etag)
	if err != nil {
		return fmt.Errorf("open conditional probe object failed: %w", err)
	}
	if body == nil {
		return fmt.Errorf("conditional probe read returned no body")
	}
	read, readErr := io.ReadAll(io.LimitReader(body, int64(len(expected)+1)))
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("read conditional probe object failed")
	}
	if entry.ETag != etag {
		return fmt.Errorf("conditional probe read returned a different ETag")
	}
	if entry.IsDir || entry.Size >= 0 && entry.Size != int64(len(expected)) {
		return fmt.Errorf("conditional probe content metadata mismatch")
	}
	if len(read) != len(expected) {
		return fmt.Errorf("conditional probe content length mismatch")
	}
	if !bytes.Equal(read, expected) {
		return fmt.Errorf("conditional probe content mismatch")
	}
	return nil
}
