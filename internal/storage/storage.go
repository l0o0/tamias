package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("资源不存在")
var ErrConflict = errors.New("资源已变化，请刷新后重试")
var ErrUnsupported = errors.New("存储服务不支持此操作")
var ErrInvalidPath = errors.New("无效路径")
var ErrInvalidRange = errors.New("无效或不可满足的范围")
var ErrPartialOperation = errors.New("操作仅部分完成")
var ErrLocked = errors.New("资源已锁定")

type Config struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Username  string `json:"username"`
	PathStyle bool   `json:"pathStyle"`
}
type Credentials struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken"`
}
type Entry struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	IsDir     bool      `json:"isDir"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	ETag      string    `json:"etag"`
	VersionID string    `json:"versionId,omitempty"`
}
type Condition struct {
	IfMatch     string
	IfNoneMatch bool
}
type Capabilities struct {
	ConditionalWrite     bool `json:"conditionalWrite"`
	ConditionalDelete    bool `json:"conditionalDelete"`
	RangeRead            bool `json:"rangeRead"`
	MultipartConditional bool `json:"multipartConditional"`
}

// Keys are relative slash paths. Implementations reject traversal and never follow redirects with credentials.
type Store interface {
	List(context.Context, string) ([]Entry, error)
	Stat(context.Context, string) (Entry, error)
	Open(context.Context, string, string) (io.ReadCloser, Entry, error)
	Put(context.Context, string, io.ReadSeeker, int64, Condition) (Entry, error)
	Delete(context.Context, string, Condition) error
	Mkdir(context.Context, string) error
}

// RangeStore opens exactly the requested byte interval from a stable object
// revision. Implementations must bind the request to etag; returning bytes
// from a different revision is a conflict, never a successful partial read.
type RangeStore interface {
	OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, Entry, error)
}

// ObjectVersion identifies one server-side object version. Delete markers are
// included in listings but cannot be opened as object bodies.
type ObjectVersion struct {
	Entry        Entry  `json:"entry"`
	VersionID    string `json:"versionId"`
	IsLatest     bool   `json:"isLatest"`
	DeleteMarker bool   `json:"deleteMarker"`
}

// DAVLock is the WebDAV-visible form of a lock held by Core's shared lock
// manager. A token scopes authority to the lock's resource and depth.
type DAVLock struct {
	Token         string
	Owner         string
	RootKey       string
	DepthInfinity bool
	Expires       time.Time
}

// VersionedStore exposes server-managed history when the provider supports it.
type VersionedStore interface {
	ListVersions(ctx context.Context, key string) ([]ObjectVersion, error)
	OpenVersion(ctx context.Context, key, versionID string) (io.ReadCloser, Entry, error)
}

// MultipartPart is the durable part receipt required to resume and complete
// a multipart upload. The caller persists these receipts alongside uploadID.
type MultipartPart struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// MultipartStore exposes resumable multipart operations. Completion must
// enforce the supplied destination condition at the final commit boundary.
type MultipartStore interface {
	CreateMultipart(ctx context.Context, key string) (uploadID string, err error)
	UploadPart(ctx context.Context, key, uploadID string, partNumber int32, body io.Reader, size int64) (MultipartPart, error)
	ListParts(ctx context.Context, key, uploadID string) ([]MultipartPart, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart, condition Condition) (Entry, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// PartialOperationError reports a multi-step mutation whose remote effects
// could not be fully rolled back, such as a MOVE whose copy succeeded but
// whose source deletion failed. Callers must not report it as an ordinary
// failure that is safe to retry blindly.
type PartialOperationError struct {
	Operation string
	Err       error
}

func (e PartialOperationError) Error() string {
	if e.Err != nil {
		return e.Operation + " partially completed: " + e.Err.Error()
	}
	return e.Operation + " partially completed"
}

func (e PartialOperationError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrPartialOperation}
	}
	return []error{ErrPartialOperation, e.Err}
}
