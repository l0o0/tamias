package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"tamiops/internal/storage"
)

var errUploadedFileMissing = errors.New("上传后未能读取远端文件；服务可能过滤了此类文件，或文件已被其他客户端移除")

// Policy grants local permission; it never changes measured server capabilities.
func canWriteStrict(c Connection) bool {
	mode, err := storage.NormalizeWriteMode(c.WriteMode)
	return err == nil && mode != storage.WriteModeCopy && c.Capabilities.ConditionalWrite
}

func canUpload(c Connection) bool {
	return canWriteStrict(c) || ordinaryWriteMode(c) && c.Tested && c.Error == ""
}

// Missing policies use the normal-sync default, including older saved configs.
func ordinaryWriteMode(c Connection) bool {
	mode, err := storage.NormalizeWriteMode(c.WriteMode)
	return err == nil && (mode == storage.WriteModeStandard || mode == storage.WriteModeCompatible)
}

func canDelete(c Connection) bool {
	return canWriteStrict(c) && c.Capabilities.ConditionalDelete
}

// Protocol gateways always need server-enforced preconditions, even when this
// connection permits explicit compatible uploads from the desktop client.
type strictBackend struct{ Backend }

func (b strictBackend) Put(ctx context.Context, key string, r io.Reader, size int64, cond storage.Condition) (storage.Entry, error) {
	return b.putStrict(ctx, key, r, size, cond, "")
}

func (b strictBackend) Mkdir(ctx context.Context, key string) error {
	return b.mkdirStrict(ctx, key)
}

type writeAccess int

const (
	writeStrict writeAccess = iota
	writeUpload
	writeCopy
	writeDelete
)

func checkWriteAccess(c Connection, access writeAccess) error {
	mode, err := storage.NormalizeWriteMode(c.WriteMode)
	if err != nil {
		return err
	}
	if access == writeCopy {
		if mode == storage.WriteModeCopy && c.Tested && c.Error == "" {
			return nil
		}
		return errors.New("请先验证连接，并选择仅另存副本模式")
	}
	if mode == storage.WriteModeCopy {
		return errors.New("仅另存副本模式只允许在文件页上传新副本，不允许覆盖、远端删除或同步写入")
	}
	if access == writeUpload && canUpload(c) || access == writeStrict && canWriteStrict(c) || access == writeDelete && canDelete(c) {
		return nil
	}
	if access == writeUpload {
		return errors.New("请先验证连接；严格保护模式还需要服务端支持条件写入")
	}
	return errors.New("此操作仍需服务端支持安全条件写入与删除，请先验证读写能力")
}

// Upload is the file-browser entry point. Stable-path callers (sync, cache,
// backups, gateways) must use Put and cannot silently adopt a new destination.
func (b Backend) Upload(ctx context.Context, key string, r io.Reader, size int64, cond storage.Condition) (storage.Entry, error) {
	if err := validKey(key); err != nil {
		return storage.Entry{}, err
	}
	if key == "" {
		return storage.Entry{}, storage.ErrInvalidPath
	}
	c, err := b.Service.connection(b.ConnectionID)
	if err != nil {
		return storage.Entry{}, err
	}
	mode, err := storage.NormalizeWriteMode(c.WriteMode)
	if err != nil {
		return storage.Entry{}, err
	}
	access := writeUpload
	if mode == storage.WriteModeCopy {
		key = copyUploadKey(key)
		cond = storage.Condition{IfNoneMatch: true}
		access = writeCopy
	} else if !cond.IfNoneMatch && cond.IfMatch == "" {
		// An explicit upload updates the selected name. Capture its version before
		// receiving the body so a change during staging still becomes a conflict.
		st, _, err := b.Service.writableWithAccess(b.ConnectionID, key, access)
		if err != nil {
			return storage.Entry{}, err
		}
		old, err := st.Stat(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			cond.IfNoneMatch = true
		} else if err != nil {
			return storage.Entry{}, err
		} else if old.IsDir || !strongTag(old.ETag) {
			return storage.Entry{}, errors.New("目标是目录或缺少可靠版本标识，无法更新")
		} else {
			cond.IfMatch = old.ETag
		}
	}
	return b.putWithAccess(ctx, key, r, size, cond, "", access, &c.Config)
}

func copyUploadKey(key string) string {
	name := path.Base(key)
	ext := path.Ext(name)
	if len(ext) > 32 {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	suffix := " (副本-" + ID() + ")" + ext
	// Keep the UTF-8 basename within common filesystem limits.
	for len(stem)+len(suffix) > 240 {
		_, width := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-width]
	}
	return path.Join(path.Dir(key), stem+suffix)
}

// A compatible commit needs evidence from the stored bytes, including providers
// that omit the PUT ETag. These checks detect conflicts but are not an atomic CAS.
func (s *Service) verifyUploadedContent(ctx context.Context, st storage.Store, key, wantHash string, wantSize int64) (storage.Entry, error) {
	before, err := st.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.Entry{}, errUploadedFileMissing
		}
		return storage.Entry{}, err
	}
	if before.IsDir || before.Size != wantSize || !strongTag(before.ETag) {
		return before, errors.New("写入后的远端版本或大小无法确认")
	}
	body, opened, err := st.Open(ctx, key, before.ETag)
	if err != nil {
		return opened, err
	}
	hasher := sha256.New()
	n, readErr := io.Copy(hasher, io.LimitReader(s.limitReader(ctx, body), wantSize+1))
	closeErr := body.Close()
	if readErr != nil {
		return opened, readErr
	}
	if closeErr != nil {
		return opened, closeErr
	}
	if opened.IsDir || opened.ETag != before.ETag || opened.Size >= 0 && opened.Size != wantSize || n != wantSize || hex.EncodeToString(hasher.Sum(nil)) != wantHash {
		return opened, errors.New("写入后的远端内容与上传内容不一致")
	}
	after, err := st.Stat(ctx, key)
	if err != nil {
		return opened, err
	}
	if after.IsDir || after.ETag != opened.ETag || after.Size != wantSize {
		return opened, errors.New("校验期间远端版本发生变化")
	}
	after.Path, after.Name = key, path.Base(key)
	return after, nil
}
