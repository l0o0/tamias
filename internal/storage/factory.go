package storage

import "fmt"

// New creates a configured remote Store. Memory fixtures are constructed with
// NewMemory so they cannot be mistaken for cloud storage.
func New(cfg Config, creds Credentials) (Store, error) {
	switch cfg.Kind {
	case "webdav":
		return newWebDAV(cfg, creds)
	case "s3":
		return newS3(cfg, creds)
	default:
		return nil, fmt.Errorf("unsupported storage kind %q", cfg.Kind)
	}
}
