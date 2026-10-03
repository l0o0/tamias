package storage

import (
	"fmt"
	"strings"
)

const maxListEntries = 10_000

// normalizeKey validates the public key format and returns its canonical form.
// It deliberately does not clean paths: cleaning would make traversal aliases
// such as "a/../b" indistinguishable from a valid key.
func normalizeKey(key string, allowRoot bool) (string, error) {
	if strings.ContainsAny(key, "\\\x00") || strings.HasPrefix(key, "/") {
		return "", ErrInvalidPath
	}
	if key == "" {
		if allowRoot {
			return "", nil
		}
		return "", ErrInvalidPath
	}
	if strings.HasSuffix(key, "/") {
		key = strings.TrimSuffix(key, "/")
		if key == "" {
			return "", ErrInvalidPath
		}
	}
	parts := strings.Split(key, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidPath
		}
	}
	return key, nil
}

func normalizePrefix(prefix string) (string, error) {
	return normalizeKey(prefix, true)
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	if key == "" {
		return prefix
	}
	return prefix + "/" + key
}

func stripPrefix(prefix, key string) (string, bool) {
	if prefix == "" {
		return key, true
	}
	if key == prefix {
		return "", true
	}
	if strings.HasPrefix(key, prefix+"/") {
		return strings.TrimPrefix(key, prefix+"/"), true
	}
	return "", false
}

func ensureNoConditionConflict(c Condition) error {
	if c.IfMatch != "" && c.IfNoneMatch {
		return fmt.Errorf("%w: If-Match 与 If-None-Match 不能同时使用", ErrConflict)
	}
	return nil
}
