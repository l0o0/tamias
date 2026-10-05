package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxDAVResponseBytes = 64 << 20

type webDAVStore struct {
	base     *url.URL
	prefix   string
	username string
	password string
	client   *http.Client
}

func newWebDAV(cfg Config, creds Credentials) (Store, error) {
	base, err := url.Parse(cfg.Endpoint)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid WebDAV endpoint")
	}
	prefix, err := normalizePrefix(cfg.Prefix)
	if err != nil {
		return nil, fmt.Errorf("invalid WebDAV prefix: %w", err)
	}
	username := creds.Username
	if username == "" {
		username = cfg.Username
	}
	return &webDAVStore{
		base:     base,
		prefix:   prefix,
		username: username,
		password: creds.Password,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (s *webDAVStore) target(key string, collection bool) (*url.URL, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return nil, err
	}
	actual := joinKey(s.prefix, key)
	segments := strings.Split(actual, "/")
	escapedSegments := make([]string, 0, len(segments))
	if actual != "" {
		for _, segment := range segments {
			escapedSegments = append(escapedSegments, url.PathEscape(segment))
		}
	}
	basePath := strings.TrimSuffix(s.base.EscapedPath(), "/")
	escapedPath := basePath + "/"
	if actual != "" {
		escapedPath += strings.Join(escapedSegments, "/")
	}
	if collection && !strings.HasSuffix(escapedPath, "/") {
		escapedPath += "/"
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, ErrInvalidPath
	}
	u := *s.base
	u.Path = decoded
	u.RawPath = escapedPath
	return &u, nil
}

func (s *webDAVStore) request(ctx context.Context, method, key string, collection bool, body io.Reader, headers http.Header) (*http.Response, error) {
	return s.requestSized(ctx, method, key, collection, body, headers, -1)
}

func (s *webDAVStore) requestSized(ctx context.Context, method, key string, collection bool, body io.Reader, headers http.Header, size int64) (*http.Response, error) {
	u, err := s.target(key, collection)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if s.username != "" || s.password != "" {
		req.SetBasicAuth(s.username, s.password)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s refused redirect (HTTP %d)", method, resp.StatusCode)
	}
	return resp, nil
}

type davMultiStatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string `xml:"href"`
	Status   string `xml:"status"`
	Propstat []struct {
		Status string  `xml:"status"`
		Prop   davProp `xml:"prop"`
	} `xml:"propstat"`
}

type davProp struct {
	ResourceType  *davResourceType `xml:"resourcetype"`
	ETag          *string          `xml:"getetag"`
	ContentLength *string          `xml:"getcontentlength"`
	LastModified  *string          `xml:"getlastmodified"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

func (s *webDAVStore) propfind(ctx context.Context, key, depth string, collection bool) ([]davResponse, error) {
	headers := make(http.Header)
	headers.Set("Depth", depth)
	headers.Set("Content-Type", "application/xml; charset=utf-8")
	body := []byte(`<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getetag/><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>`)
	resp, err := s.request(ctx, "PROPFIND", key, collection, bytes.NewReader(body), headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 207 {
		return nil, statusError("PROPFIND", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDAVResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read WebDAV response: %w", err)
	}
	if len(data) > maxDAVResponseBytes {
		return nil, fmt.Errorf("WebDAV response exceeds %d bytes", maxDAVResponseBytes)
	}
	var result davMultiStatus
	if err := xml.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode WebDAV multistatus XML")
	}
	for _, response := range result.Responses {
		if strings.TrimSpace(response.Href) == "" {
			return nil, fmt.Errorf("incomplete WebDAV multistatus: response has no href")
		}
		if response.Status != "" {
			code, ok := statusCode(response.Status)
			if !ok {
				return nil, fmt.Errorf("incomplete WebDAV multistatus: invalid response status")
			}
			if code < 200 || code >= 300 {
				return nil, statusError("PROPFIND response", code)
			}
		}
		if len(response.Propstat) == 0 {
			return nil, fmt.Errorf("incomplete WebDAV multistatus: response has no propstat")
		}
		var successful davProp
		var missing []davProp
		for _, propstat := range response.Propstat {
			code, ok := statusCode(propstat.Status)
			if !ok {
				return nil, fmt.Errorf("incomplete WebDAV multistatus: invalid propstat status")
			}
			if code < 200 || code >= 300 {
				if code == http.StatusNotFound {
					missing = append(missing, propstat.Prop)
					continue
				}
				return nil, statusError("PROPFIND property", code)
			}
			mergeDAVProp(&successful, propstat.Prop)
		}
		if successful.ResourceType == nil {
			return nil, fmt.Errorf("incomplete WebDAV multistatus: required resourcetype is missing")
		}
		isCollection := successful.ResourceType.Collection != nil
		for _, prop := range missing {
			if !isOptionalDAVPropstat(prop, isCollection) {
				return nil, statusError("PROPFIND property", http.StatusNotFound)
			}
		}
	}
	return result.Responses, nil
}

func isOptionalDAVPropstat(prop davProp, isCollection bool) bool {
	// Resource type is required to classify the entry. Length and modification
	// time only enrich it, while a collection's ETag is not needed for file
	// conditional-write safety. File ETags remain required.
	if prop.ResourceType != nil {
		return false
	}
	if prop.ETag != nil && !isCollection {
		return false
	}
	return prop.ETag != nil || prop.ContentLength != nil || prop.LastModified != nil
}

func mergeDAVProp(dst *davProp, src davProp) {
	if src.ResourceType != nil {
		dst.ResourceType = src.ResourceType
	}
	if src.ETag != nil {
		dst.ETag = src.ETag
	}
	if src.ContentLength != nil {
		dst.ContentLength = src.ContentLength
	}
	if src.LastModified != nil {
		dst.LastModified = src.LastModified
	}
}

func statusError(op string, status int) error {
	switch status {
	case http.StatusPreconditionFailed, http.StatusConflict:
		return fmt.Errorf("%s: %w (HTTP %d)", op, ErrConflict, status)
	case http.StatusNotFound:
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	default:
		return fmt.Errorf("%s failed (HTTP %d)", op, status)
	}
}

func (s *webDAVStore) entryFromResponse(key string, response davResponse) (Entry, bool, error) {
	if !statusOK(response.Status) && response.Status != "" {
		if strings.Contains(response.Status, " 404 ") {
			return Entry{}, false, ErrNotFound
		}
		return Entry{}, false, nil
	}
	var prop davProp
	found := false
	for _, propstat := range response.Propstat {
		if statusOK(propstat.Status) {
			mergeDAVProp(&prop, propstat.Prop)
			found = true
		}
	}
	if !found {
		return Entry{}, false, nil
	}
	e := Entry{Path: key, Name: path.Base(key)}
	if key == "" {
		e.Name = ""
	}
	if prop.ResourceType == nil {
		return Entry{}, false, fmt.Errorf("required WebDAV resourcetype is missing")
	}
	if prop.ETag != nil {
		e.ETag = strings.TrimSpace(*prop.ETag)
	}
	if prop.ResourceType.Collection != nil {
		e.IsDir = true
	}
	if prop.ContentLength != nil && *prop.ContentLength != "" {
		size, err := strconv.ParseInt(strings.TrimSpace(*prop.ContentLength), 10, 64)
		if err != nil || size < 0 {
			return Entry{}, false, fmt.Errorf("invalid WebDAV content length")
		}
		e.Size = size
	}
	if prop.LastModified != nil && *prop.LastModified != "" {
		t, err := http.ParseTime(strings.TrimSpace(*prop.LastModified))
		if err == nil {
			e.Modified = t
		}
	}
	return e, true, nil
}

func statusOK(status string) bool {
	code, ok := statusCode(status)
	return ok && code >= 200 && code < 300
}

func statusCode(status string) (int, bool) {
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return 0, false
	}
	code, err := strconv.Atoi(fields[1])
	return code, err == nil && code >= 100 && code <= 599
}

func (s *webDAVStore) responseKey(requestURL *url.URL, href string) (string, bool, error) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", false, fmt.Errorf("missing href")
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", false, err
	}
	if strings.Contains(strings.ToLower(ref.RawPath), "%2f") {
		return "", false, ErrInvalidPath
	}
	u := requestURL.ResolveReference(ref)
	if !strings.EqualFold(u.Scheme, requestURL.Scheme) || !strings.EqualFold(u.Host, requestURL.Host) {
		return "", false, nil
	}
	basePath := strings.TrimSuffix(s.base.Path, "/")
	hrefPath := u.Path
	var actual string
	if basePath == "" {
		actual = strings.TrimPrefix(hrefPath, "/")
	} else if hrefPath == basePath {
		actual = ""
	} else if strings.HasPrefix(hrefPath, basePath+"/") {
		actual = strings.TrimPrefix(hrefPath, basePath+"/")
	} else {
		return "", false, nil
	}
	actual = strings.TrimSuffix(actual, "/")
	canonical, err := normalizeKey(actual, true)
	if err != nil {
		return "", false, err
	}
	return canonical, true, nil
}

func (s *webDAVStore) List(ctx context.Context, key string) ([]Entry, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return nil, err
	}
	requestURL, err := s.target(key, true)
	if err != nil {
		return nil, err
	}
	responses, err := s.propfind(ctx, key, "1", true)
	if err != nil {
		return nil, err
	}
	children := make(map[string]Entry)
	seenResponses := make(map[string]struct{}, len(responses))
	rootFound := false
	for _, response := range responses {
		actual, ok, err := s.responseKey(requestURL, response.Href)
		if err != nil {
			return nil, fmt.Errorf("invalid WebDAV href: %w", err)
		}
		if !ok {
			continue
		}
		if _, duplicate := seenResponses[actual]; duplicate {
			return nil, fmt.Errorf("incomplete WebDAV listing: duplicate response for %q", actual)
		}
		seenResponses[actual] = struct{}{}
		relative, ok := stripPrefix(s.prefix, actual)
		if !ok {
			continue
		}
		if relative == key {
			root, found, rootErr := s.entryFromResponse(key, response)
			if rootErr != nil {
				return nil, fmt.Errorf("incomplete WebDAV listing root: %w", rootErr)
			}
			if !found || !root.IsDir {
				return nil, fmt.Errorf("incomplete WebDAV listing: requested resource is not a confirmed collection")
			}
			rootFound = true
			continue // Depth 1 commonly includes the collection itself.
		}
		prefix := key
		if prefix != "" {
			prefix += "/"
		}
		if !strings.HasPrefix(relative, prefix) {
			continue
		}
		rest := strings.TrimPrefix(relative, prefix)
		first, _, nested := strings.Cut(rest, "/")
		if first == "" {
			continue
		}
		childPath := joinKey(key, first)
		entry, found, err := s.entryFromResponse(childPath, response)
		if err != nil {
			return nil, fmt.Errorf("incomplete WebDAV listing child %q: %w", childPath, err)
		}
		if !found {
			return nil, fmt.Errorf("incomplete WebDAV listing: child %q has no readable properties", childPath)
		}
		if nested {
			entry.Path, entry.Name, entry.IsDir, entry.Size = childPath, first, true, 0
		}
		children[childPath] = entry
		if len(children) > maxListEntries {
			return nil, fmt.Errorf("WebDAV listing exceeds %d entries", maxListEntries)
		}
	}
	if !rootFound {
		return nil, fmt.Errorf("incomplete WebDAV listing: response omitted the requested collection")
	}
	result := make([]Entry, 0, len(children))
	for _, entry := range children {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *webDAVStore) Stat(ctx context.Context, key string) (Entry, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return Entry{}, err
	}
	requestURL, err := s.target(key, true)
	if err != nil {
		return Entry{}, err
	}
	responses, err := s.propfind(ctx, key, "0", false)
	if err != nil {
		return Entry{}, err
	}
	for _, response := range responses {
		actual, ok, err := s.responseKey(requestURL, response.Href)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid WebDAV href: %w", err)
		}
		relative, inPrefix := stripPrefix(s.prefix, actual)
		if !ok || !inPrefix || relative != key {
			continue
		}
		entry, found, err := s.entryFromResponse(key, response)
		if err != nil {
			return Entry{}, err
		}
		if found {
			return entry, nil
		}
	}
	return Entry{}, ErrNotFound
}

func (s *webDAVStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	headers := make(http.Header)
	headers.Set("Accept-Encoding", "identity")
	if etag != "" {
		headers.Set("If-Match", etag)
	}
	resp, err := s.request(ctx, http.MethodGet, key, false, nil, headers)
	if err != nil {
		return nil, Entry{}, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusPreconditionFailed {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, Entry{}, ErrNotFound
		}
		return nil, Entry{}, ErrConflict
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, Entry{}, statusError("GET", resp.StatusCode)
	}
	eTag := resp.Header.Get("ETag")
	if etag != "" && eTag != etag {
		_ = resp.Body.Close()
		return nil, Entry{}, ErrConflict
	}
	e := Entry{Path: key, Name: path.Base(key), Size: resp.ContentLength, ETag: eTag, Modified: parseHTTPTime(resp.Header.Get("Last-Modified"))}
	return resp.Body, e, nil
}

func (s *webDAVStore) OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	if etag == "" || strings.HasPrefix(strings.TrimSpace(etag), "W/") || start < 0 || length <= 0 || start > int64(^uint64(0)>>1)-length {
		return nil, Entry{}, ErrUnsupported
	}
	end := start + length - 1
	headers := make(http.Header)
	headers.Set("Accept-Encoding", "identity")
	headers.Set("If-Match", etag)
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := s.request(ctx, http.MethodGet, key, false, nil, headers)
	if err != nil {
		return nil, Entry{}, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, Entry{}, ErrNotFound
	case http.StatusPreconditionFailed:
		_ = resp.Body.Close()
		return nil, Entry{}, ErrConflict
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return nil, Entry{}, ErrInvalidRange
	case http.StatusOK:
		_ = resp.Body.Close()
		return nil, Entry{}, ErrUnsupported
	case http.StatusPartialContent:
	default:
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotImplemented || resp.StatusCode == http.StatusMethodNotAllowed {
			return nil, Entry{}, ErrUnsupported
		}
		return nil, Entry{}, statusError("range GET", resp.StatusCode)
	}
	if resp.Header.Get("ETag") != etag || !validWebDAVContentRange(resp.Header.Get("Content-Range"), start, end) || resp.ContentLength != length {
		_ = resp.Body.Close()
		return nil, Entry{}, ErrConflict
	}
	e := Entry{Path: key, Name: path.Base(key), Size: length, ETag: resp.Header.Get("ETag"), Modified: parseHTTPTime(resp.Header.Get("Last-Modified"))}
	return resp.Body, e, nil
}

func validWebDAVContentRange(value string, start, end int64) bool {
	prefix := fmt.Sprintf("bytes %d-%d/", start, end)
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(value, prefix), 10, 64)
	return err == nil && total > end
}

func parseHTTPTime(value string) time.Time {
	t, _ := http.ParseTime(value)
	return t
}

func (s *webDAVStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
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
	headers := make(http.Header)
	if condition.IfMatch != "" {
		headers.Set("If-Match", condition.IfMatch)
	}
	if condition.IfNoneMatch {
		headers.Set("If-None-Match", "*")
	}
	resp, err := s.requestSized(ctx, http.MethodPut, key, false, body, headers, size)
	if err != nil {
		return Entry{}, err
	}
	if err := resp.Body.Close(); err != nil {
		return Entry{}, fmt.Errorf("close WebDAV PUT response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return Entry{}, statusError("PUT", resp.StatusCode)
	}
	entry := Entry{Path: key, Name: path.Base(key), ETag: resp.Header.Get("ETag"), Modified: parseHTTPTime(resp.Header.Get("Last-Modified")), Size: size}
	return entry, nil
}

func (s *webDAVStore) Delete(ctx context.Context, key string, condition Condition) error {
	key, err := normalizeKey(key, false)
	if err != nil {
		return err
	}
	if err := ensureNoConditionConflict(condition); err != nil {
		return err
	}
	headers := make(http.Header)
	if condition.IfMatch != "" {
		headers.Set("If-Match", condition.IfMatch)
	}
	if condition.IfNoneMatch {
		headers.Set("If-None-Match", "*")
	}
	resp, err := s.request(ctx, http.MethodDelete, key, false, nil, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		return nil
	}
	return statusError("DELETE", resp.StatusCode)
}

func (s *webDAVStore) Mkdir(ctx context.Context, key string) error {
	key, err := normalizeKey(key, true)
	if err != nil {
		return err
	}
	if key == "" {
		return nil
	}
	resp, err := s.request(ctx, "MKCOL", key, true, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return statusError("MKCOL", resp.StatusCode)
}

var _ Store = (*webDAVStore)(nil)
