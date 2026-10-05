package core

import (
	"path"
	"strings"
)

// syncPathExcluded reports paths excluded by the job's configured patterns and
// the built-in macOS metadata rule. The built-in rule is deliberately limited
// to files named exactly .DS_Store, wherever they appear in the tree.
func syncPathExcluded(patterns []string, key string) bool {
	return isSyncMetadataPath(key) || excludedPath(patterns, key)
}

func isSyncMetadataPath(key string) bool {
	key = strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	return key != "" && path.Base(key) == ".DS_Store"
}
