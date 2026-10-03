// Package gateway exposes the storage core through a small, authenticated
// WebDAV-compatible HTTP surface.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"tamiops/internal/storage"
)

// Backend is the narrow Core interface used by the HTTP gateway. The gateway
// never accesses a storage provider directly.
type Backend interface {
	List(ctx context.Context, key string) ([]storage.Entry, error)
	Stat(ctx context.Context, key string) (storage.Entry, error)
	Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error)
	Put(ctx context.Context, key string, body io.Reader, size int64, cond storage.Condition) (storage.Entry, error)
	Delete(ctx context.Context, key string, cond storage.Condition) error
	Mkdir(ctx context.Context, key string) error
}

type RangeBackend interface {
	OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, storage.Entry, error)
}

type RangeCapabilityBackend interface {
	SupportsRangeRead() bool
}

type CopyMoveBackend interface {
	Copy(ctx context.Context, src, dst string, overwrite bool) (storage.Entry, error)
	Move(ctx context.Context, src, dst string, overwrite bool) (storage.Entry, error)
	DeleteTree(ctx context.Context, key string) error
}

type DAVLockBackend interface {
	AcquireDAVLock(ctx context.Context, key, owner string, depth bool, timeout time.Duration) (token string, expires time.Time, err error)
	RefreshDAVLock(ctx context.Context, key, token string, timeout time.Duration) (expires time.Time, err error)
	UnlockDAVLock(ctx context.Context, key, token string) error
	DAVLocks(ctx context.Context, key string) ([]storage.DAVLock, error)
}

type AccessEvent struct {
	At       time.Time
	Method   string
	Path     string
	Status   int
	BytesIn  int64
	BytesOut int64
	Duration time.Duration
	Partial  bool
}

type AccessStats struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	BytesIn  int64 `json:"bytesIn"`
	BytesOut int64 `json:"bytesOut"`
	Active   int64 `json:"active"`
}

// GatewayHandler exposes bounded access statistics in addition to HTTP serving.
type GatewayHandler interface {
	http.Handler
	Stats() AccessStats
}

type Config struct {
	Username string
	Password string
	Prefix   string
	ReadOnly bool
	OnAccess func(AccessEvent)
}

const maxPropfindEntries = 1000

const basicSupportedMethods = "OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE"

const maxDAVLockTimeout = time.Hour

type handler struct {
	cfg       Config
	backend   Backend
	prefix    string
	configErr bool
	slots     chan struct{}
	requests  atomic.Int64
	errors    atomic.Int64
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	active    atomic.Int64
}

// NewHandler returns an authenticated WebDAV HTTP handler. Prefix is a
// storage-key prefix; it is never included in a response href.
func NewHandler(cfg Config, backend Backend) GatewayHandler {
	prefix, ok := normalizePrefix(cfg.Prefix)
	return &handler{cfg: cfg, backend: backend, prefix: prefix, configErr: !ok, slots: make(chan struct{}, 8)}
}

func normalizePrefix(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	value = strings.Trim(value, "/")
	if value == "" {
		return "", true
	}
	if strings.ContainsAny(value, "\\\x00") {
		return "", false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return value, true
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.errors.Add(1)
		http.Error(w, "gateway busy", http.StatusServiceUnavailable)
		return
	}
	h.active.Add(1)
	started := time.Now()
	response := &accessResponseWriter{ResponseWriter: w}
	requestBody := &accessRequestBody{ReadCloser: r.Body}
	if r.Body != nil {
		r.Body = requestBody
	}
	w = response
	defer func() {
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= http.StatusBadRequest || response.partial {
			h.errors.Add(1)
		}
		h.bytesIn.Add(requestBody.bytes)
		h.bytesOut.Add(response.bytes)
		event := AccessEvent{At: started.UTC(), Method: r.Method, Path: r.URL.EscapedPath(), Status: status, BytesIn: requestBody.bytes, BytesOut: response.bytes, Duration: time.Since(started), Partial: response.partial}
		if h.cfg.OnAccess != nil {
			func() {
				defer func() { _ = recover() }()
				h.cfg.OnAccess(event)
			}()
		}
		h.active.Add(-1)
	}()
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if h.configErr || h.backend == nil {
		http.Error(w, "gateway is not configured", http.StatusInternalServerError)
		return
	}
	tokens, err := parseSimpleIf(r.Header.Values("If"))
	if err != nil {
		http.Error(w, "complex WebDAV If conditions are not supported", http.StatusNotImplemented)
		return
	}
	r = r.WithContext(storage.WithLockTokens(r.Context(), tokens))
	if r.Method == "MKCOL" && (len(r.Header.Values("If-Match")) > 0 || len(r.Header.Values("If-None-Match")) > 0) {
		http.Error(w, "MKCOL conditions are not supported", http.StatusNotImplemented)
		return
	}
	allow := h.allowedMethods()
	if h.cfg.ReadOnly {
		allow = "OPTIONS, PROPFIND, GET, HEAD"
	}
	switch r.Method {
	case http.MethodOptions:
		if _, _, err := parseRelativePath(r); err != nil {
			writeStorageError(w, err)
			return
		}
		w.Header().Set("Allow", allow)
		w.Header().Set("DAV", "1")
		w.WriteHeader(http.StatusOK)
	case "PROPFIND":
		h.propfind(w, r)
	case http.MethodGet, http.MethodHead:
		h.getOrHead(w, r)
	case http.MethodPut:
		h.put(w, r)
	case "MKCOL":
		h.mkcol(w, r)
	case http.MethodDelete:
		h.delete(w, r)
	case "COPY":
		h.copyOrMove(w, r, false)
	case "MOVE":
		h.copyOrMove(w, r, true)
	case "LOCK":
		h.lock(w, r)
	case "UNLOCK":
		h.unlock(w, r)
	case "PROPPATCH":
		h.propPatch(w, r)
	default:
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) allowedMethods() string {
	methods := basicSupportedMethods
	if !h.cfg.ReadOnly {
		if _, ok := h.backend.(CopyMoveBackend); ok {
			methods += ", COPY, MOVE"
		}
		if _, ok := h.backend.(DAVLockBackend); ok {
			methods += ", LOCK, UNLOCK"
		}
	}
	return methods
}

func (h *handler) Stats() AccessStats {
	return AccessStats{Requests: h.requests.Load(), Errors: h.errors.Load(), BytesIn: h.bytesIn.Load(), BytesOut: h.bytesOut.Load(), Active: h.active.Load()}
}

func (h *handler) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.Username))
	passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.Password))
	return userMatch&passMatch == 1
}

func parseRelativePath(r *http.Request) (rel string, trailingSlash bool, err error) {
	escaped := r.URL.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	return parseEscapedPath(escaped)
}

func parseEscapedPath(escaped string) (rel string, trailingSlash bool, err error) {
	if !strings.HasPrefix(escaped, "/") || hasEncodedSeparator(escaped) {
		return "", false, storage.ErrInvalidPath
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil || !utf8.ValidString(decoded) || strings.ContainsAny(decoded, "\\\x00") {
		return "", false, storage.ErrInvalidPath
	}
	if decoded != "/" && strings.HasPrefix(decoded, "//") {
		return "", false, storage.ErrInvalidPath
	}
	trailingSlash = strings.HasSuffix(decoded, "/")
	trimmed := strings.TrimPrefix(decoded, "/")
	if trailingSlash {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	if trimmed == "" {
		return "", trailingSlash, nil
	}
	parts := strings.Split(trimmed, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false, storage.ErrInvalidPath
		}
	}
	return strings.Join(parts, "/"), trailingSlash, nil
}

func hasEncodedSeparator(value string) bool {
	for i := 0; i+2 < len(value); i++ {
		if value[i] != '%' {
			continue
		}
		if (value[i+1] == '2' && (value[i+2] == 'f' || value[i+2] == 'F')) ||
			(value[i+1] == '5' && (value[i+2] == 'c' || value[i+2] == 'C')) {
			return true
		}
	}
	return false
}

func (h *handler) key(rel string) string {
	if h.prefix == "" {
		return rel
	}
	if rel == "" {
		return h.prefix
	}
	return h.prefix + "/" + rel
}

func (h *handler) propfind(w http.ResponseWriter, r *http.Request) {
	rel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	depth := strings.TrimSpace(r.Header.Get("Depth"))
	if depth == "" {
		depth = "0"
	}
	if depth == "infinity" {
		http.Error(w, "infinite-depth PROPFIND is not supported", http.StatusForbidden)
		return
	}
	if depth != "0" && depth != "1" {
		http.Error(w, "Depth must be 0 or 1", http.StatusBadRequest)
		return
	}
	includeLockProps, err := requestsLockProperties(r.Body)
	if err != nil {
		http.Error(w, "invalid PROPFIND request body", http.StatusBadRequest)
		return
	}

	key := h.key(rel)
	entry, err := h.backend.Stat(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			if locks, ok := h.backend.(DAVLockBackend); ok {
				active, lockErr := locks.DAVLocks(r.Context(), key)
				if lockErr == nil && len(active) > 0 {
					entry = storage.Entry{Path: key, Name: baseNameKey(key)}
					err = nil
				}
			}
		}
		if err != nil {
			writeStorageError(w, err)
			return
		}
	}
	if !entry.IsDir {
		// A file has no children, but it is still a valid depth-0 resource.
		depth = "0"
	}
	entries := []davEntry{{rel: rel, entry: entry}}
	if depth == "1" {
		children, listErr := h.backend.List(r.Context(), key)
		if listErr != nil {
			writeStorageError(w, listErr)
			return
		}
		seen := map[string]struct{}{rel: {}}
		for _, child := range children {
			childRel, ok := childRelative(child, rel, h.prefix)
			if !ok || childRel == rel {
				continue
			}
			if _, found := seen[childRel]; found {
				continue
			}
			seen[childRel] = struct{}{}
			entries = append(entries, davEntry{rel: childRel, entry: child})
			if len(entries) > maxPropfindEntries {
				http.Error(w, "PROPFIND result exceeds the configured limit", 507)
				return
			}
		}
	}
	sort.Slice(entries[1:], func(i, j int) bool {
		return entries[1+i].rel < entries[1+j].rel
	})

	response := davMultistatus{XMLNS: "DAV:"}
	for _, item := range entries {
		resourceResponse := makeDAVResponse(item.rel, item.entry)
		if includeLockProps {
			if err := h.addLockProperties(r.Context(), h.key(item.rel), item.rel, &resourceResponse); err != nil {
				writeStorageError(w, err)
				return
			}
		}
		response.Responses = append(response.Responses, resourceResponse)
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(response); err != nil {
		return
	}
}

type davEntry struct {
	rel   string
	entry storage.Entry
}

func childRelative(entry storage.Entry, parent, prefix string) (string, bool) {
	candidate := strings.TrimSuffix(entry.Path, "/")
	if candidate == "" {
		if !validChildName(entry.Name) {
			return "", false
		}
		candidate = joinRel(parent, entry.Name)
	} else {
		candidate = strings.TrimPrefix(candidate, "/")
		if strings.ContainsAny(candidate, "\\\x00") {
			return "", false
		}
		if prefix != "" {
			switch {
			case candidate == prefix:
				candidate = ""
			case strings.HasPrefix(candidate, prefix+"/"):
				candidate = strings.TrimPrefix(candidate, prefix+"/")
			case parent != "" && (candidate == parent || strings.HasPrefix(candidate, parent+"/")):
				// Some Core adapters return paths relative to Prefix.
			default:
				candidate = joinRel(parent, candidate)
			}
		} else if parent != "" && (candidate == parent || strings.HasPrefix(candidate, parent+"/")) {
			// List may return full gateway-relative keys.
		} else {
			candidate = joinRel(parent, candidate)
		}
	}
	if candidate == "" || strings.ContainsAny(candidate, "\\\x00") {
		return "", false
	}
	for _, part := range strings.Split(candidate, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	if parent != "" {
		if !strings.HasPrefix(candidate, parent+"/") {
			return "", false
		}
		candidate = strings.TrimPrefix(candidate, parent+"/")
	}
	if candidate == "" || strings.Contains(candidate, "/") {
		return "", false
	}
	return joinRel(parent, candidate), true
}

func validChildName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func joinRel(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

type davMultistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	XMLNS     string        `xml:"xmlns,attr"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string      `xml:"href"`
	Propstat davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProperties `xml:"prop"`
	Status string        `xml:"status"`
}

type davProperties struct {
	ResourceType  davResourceType   `xml:"resourcetype"`
	ContentLength int64             `xml:"getcontentlength"`
	LastModified  string            `xml:"getlastmodified,omitempty"`
	ETag          string            `xml:"getetag,omitempty"`
	SupportedLock *davSupportedLock `xml:"supportedlock,omitempty"`
	LockDiscovery *davLockDiscovery `xml:"lockdiscovery,omitempty"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection,omitempty"`
}

func makeDAVResponse(rel string, entry storage.Entry) davResponse {
	href := hrefFor(rel, entry.IsDir)
	props := davProperties{ContentLength: entry.Size}
	if entry.IsDir {
		props.ResourceType.Collection = &struct{}{}
	}
	if !entry.Modified.IsZero() {
		props.LastModified = entry.Modified.UTC().Format(http.TimeFormat)
	}
	if etag := formatETag(entry.ETag); etag != "" {
		props.ETag = etag
	}
	return davResponse{
		Href: href,
		Propstat: davPropstat{
			Prop:   props,
			Status: "HTTP/1.1 200 OK",
		},
	}
}

func hrefFor(rel string, directory bool) string {
	if rel == "" {
		return "/"
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	href := "/" + strings.Join(parts, "/")
	if directory {
		href += "/"
	}
	return href
}

func (h *handler) getOrHead(w http.ResponseWriter, r *http.Request) {
	rel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	key := h.key(rel)
	entry, err := h.backend.Stat(r.Context(), key)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if entry.IsDir {
		http.Error(w, "directories cannot be downloaded", http.StatusMethodNotAllowed)
		return
	}
	_, rangeSupported := h.backend.(RangeBackend)
	if capabilities, ok := h.backend.(RangeCapabilityBackend); ok {
		rangeSupported = rangeSupported && capabilities.SupportsRangeRead()
	}
	rangeSupported = rangeSupported && entry.Size >= 0 && formatETag(entry.ETag) != "" && !entityTagIsWeak(entry.ETag)
	setObjectHeaders(w.Header(), entry, rangeSupported)
	status, condErr := evaluateReadConditions(r, entry)
	if condErr != nil {
		http.Error(w, "invalid conditional request", http.StatusBadRequest)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	start, length, useRange, unsatisfiable := int64(0), int64(0), false, false
	if rangeSupported && hasHeader(r, "Range") && ifRangeAllows(r, entry) {
		start, length, useRange, unsatisfiable = parseByteRange(strings.Join(r.Header.Values("Range"), ","), entry.Size)
	}
	if unsatisfiable {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	var body io.ReadCloser
	var opened storage.Entry
	if useRange {
		rangeBackend := h.backend.(RangeBackend)
		body, opened, err = rangeBackend.OpenRange(r.Context(), key, entry.ETag, start, length)
	} else {
		body, opened, err = h.backend.Open(r.Context(), key, entry.ETag)
	}
	if err != nil {
		if useRange && errors.Is(err, storage.ErrInvalidRange) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.Size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		writeStorageError(w, err)
		return
	}
	if body == nil {
		http.Error(w, "storage returned no object body", http.StatusBadGateway)
		return
	}
	defer body.Close()
	if entry.ETag != "" && opened.ETag != entry.ETag || opened.IsDir {
		http.Error(w, "storage returned a different object revision", http.StatusBadGateway)
		return
	}
	if useRange {
		if opened.Size != length {
			http.Error(w, "storage returned an invalid range", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, entry.Size))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "application/octet-stream")
		if etag := formatETag(opened.ETag); etag != "" {
			w.Header().Set("ETag", etag)
		}
		if !opened.Modified.IsZero() {
			w.Header().Set("Last-Modified", opened.Modified.UTC().Format(http.TimeFormat))
		}
		w.WriteHeader(http.StatusPartialContent)
		written, copyErr := io.Copy(w, body)
		if copyErr != nil || written != length {
			markAccessPartial(w)
		}
		return
	}
	setObjectHeaders(w.Header(), opened, rangeSupported)
	w.WriteHeader(http.StatusOK)
	written, copyErr := io.Copy(w, body)
	if copyErr != nil || written != opened.Size {
		markAccessPartial(w)
	}
}

func setObjectHeaders(header http.Header, entry storage.Entry, rangeSupported bool) {
	header.Set("Content-Length", fmt.Sprintf("%d", entry.Size))
	if rangeSupported {
		header.Set("Accept-Ranges", "bytes")
	} else {
		header.Set("Accept-Ranges", "none")
	}
	if etag := formatETag(entry.ETag); etag != "" {
		header.Set("ETag", etag)
	}
	if !entry.Modified.IsZero() {
		header.Set("Last-Modified", entry.Modified.UTC().Format(http.TimeFormat))
	}
	header.Set("Content-Type", "application/octet-stream")
}

func evaluateReadConditions(r *http.Request, entry storage.Entry) (int, error) {
	current := etagValue(entry.ETag)
	if values := r.Header.Values("If-Match"); len(values) > 0 {
		tags, wildcard, err := parseEntityTags(strings.Join(values, ","))
		if err != nil {
			return 0, err
		}
		matched := wildcard
		for _, tag := range tags {
			if current != "" && !entityTagIsWeak(entry.ETag) && !tag.weak && tag.value == current {
				matched = true
			}
		}
		if !matched {
			return http.StatusPreconditionFailed, nil
		}
	}
	if values := r.Header.Values("If-None-Match"); len(values) > 0 {
		tags, wildcard, err := parseEntityTags(strings.Join(values, ","))
		if err != nil {
			return 0, err
		}
		matched := wildcard
		for _, tag := range tags {
			if current != "" && tag.value == current {
				matched = true
			}
		}
		if matched {
			return http.StatusNotModified, nil
		}
	} else if value := r.Header.Get("If-Modified-Since"); value != "" && !entry.Modified.IsZero() {
		date, err := http.ParseTime(value)
		if err == nil && !entry.Modified.Truncate(time.Second).After(date) {
			return http.StatusNotModified, nil
		}
	}
	if len(r.Header.Values("If-Match")) == 0 {
		if value := r.Header.Get("If-Unmodified-Since"); value != "" && !entry.Modified.IsZero() {
			date, err := http.ParseTime(value)
			if err == nil && entry.Modified.Truncate(time.Second).After(date) {
				return http.StatusPreconditionFailed, nil
			}
		}
	}
	return 0, nil
}

func ifRangeAllows(r *http.Request, entry storage.Entry) bool {
	values := r.Header.Values("If-Range")
	if len(values) > 1 {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("If-Range"))
	if value == "" {
		return true
	}
	if strings.HasPrefix(value, "W/") {
		return false
	}
	if strings.HasPrefix(value, "\"") {
		tags, wildcard, err := parseEntityTags(value)
		return err == nil && !wildcard && len(tags) == 1 && !tags[0].weak && !entityTagIsWeak(entry.ETag) && etagValue(entry.ETag) == tags[0].value
	}
	date, err := http.ParseTime(value)
	return err == nil && !entry.Modified.IsZero() && !entry.Modified.Truncate(time.Second).After(date)
}

// parseByteRange accepts one RFC byte range. Invalid or multi-range values are
// ignored by the caller; a valid but unsatisfiable range receives 416.
func parseByteRange(value string, size int64) (start, length int64, useRange, unsatisfiable bool) {
	unit, spec, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") || strings.Contains(spec, ",") || size < 0 {
		return 0, 0, false, false
	}
	spec = strings.TrimSpace(spec)
	first, last, ok := strings.Cut(spec, "-")
	if !ok || strings.Contains(last, "-") {
		return 0, 0, false, false
	}
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		if size == 0 {
			return 0, 0, false, true
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, true, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	if start >= size {
		return 0, 0, false, true
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, true, false
}

type entityTag struct {
	value string
	weak  bool
}

func parseEntityTags(value string) ([]entityTag, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false, errors.New("empty entity tag")
	}
	if value == "*" {
		return nil, true, nil
	}
	var tags []entityTag
	for len(value) > 0 {
		value = strings.TrimLeft(value, " \t")
		weak := false
		if strings.HasPrefix(value, "W/") {
			weak = true
			value = value[2:]
		}
		if len(value) == 0 || value[0] != '"' {
			return nil, false, errors.New("invalid entity tag")
		}
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			return nil, false, errors.New("invalid entity tag")
		}
		opaque := value[1 : end+1]
		for i := 0; i < len(opaque); i++ {
			if opaque[i] < 0x21 || opaque[i] == 0x7f {
				return nil, false, errors.New("invalid entity tag")
			}
		}
		tags = append(tags, entityTag{value: opaque, weak: weak})
		value = strings.TrimLeft(value[end+2:], " \t")
		if value == "" {
			break
		}
		if value[0] != ',' {
			return nil, false, errors.New("invalid entity tag list")
		}
		value = value[1:]
		if strings.TrimSpace(value) == "" {
			return nil, false, errors.New("invalid entity tag list")
		}
	}
	if len(tags) == 0 {
		return nil, false, errors.New("empty entity tag list")
	}
	return tags, false, nil
}

func etagValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimPrefix(value, "W/")
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func entityTagIsWeak(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "W/")
}

func formatETag(value string) string {
	if value == "" {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	weakPrefix := ""
	if strings.HasPrefix(trimmed, "W/") {
		weakPrefix = "W/"
		trimmed = strings.TrimPrefix(trimmed, "W/")
	}
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		if _, _, err := parseEntityTags(trimmed); err == nil {
			return weakPrefix + trimmed
		}
		return ""
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < 0x21 || trimmed[i] == '"' || trimmed[i] == 0x7f {
			return ""
		}
	}
	return weakPrefix + `"` + trimmed + `"`
}

func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	rel, trailing, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if rel == "" || trailing {
		http.Error(w, "PUT requires a file path", http.StatusMethodNotAllowed)
		return
	}
	key := h.key(rel)
	cond, err := h.writeCondition(r.Context(), key, r)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	// Core owns body collection, completeness checks, and commit. The exact
	// Content-Length from net/http is passed through, including -1 when unknown.
	_, statErr := h.backend.Stat(r.Context(), key)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		writeStorageError(w, statErr)
		return
	}
	committed, err := h.backend.Put(r.Context(), key, r.Body, r.ContentLength, cond)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if etag := formatETag(committed.ETag); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if !committed.Modified.IsZero() {
		w.Header().Set("Last-Modified", committed.Modified.UTC().Format(http.TimeFormat))
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (h *handler) writeCondition(ctx context.Context, key string, r *http.Request) (storage.Condition, error) {
	var cond storage.Condition
	if values := r.Header.Values("If-Match"); len(values) > 0 {
		tags, wildcard, err := parseEntityTags(strings.Join(values, ","))
		if err != nil {
			return cond, handlerError{status: http.StatusBadRequest, message: "invalid If-Match header"}
		}
		switch {
		case wildcard:
			current, statErr := h.backend.Stat(ctx, key)
			if statErr != nil {
				if errors.Is(statErr, storage.ErrNotFound) {
					return cond, storage.ErrConflict
				}
				return cond, statErr
			}
			if current.ETag == "" || entityTagIsWeak(current.ETag) {
				return cond, storage.ErrConflict
			}
			cond.IfMatch = current.ETag
		case len(tags) == 1 && tags[0].weak:
			return cond, storage.ErrConflict
		case len(tags) == 1:
			current, statErr := h.backend.Stat(ctx, key)
			if statErr != nil {
				if errors.Is(statErr, storage.ErrNotFound) {
					return cond, storage.ErrConflict
				}
				return cond, statErr
			}
			if current.ETag == "" || entityTagIsWeak(current.ETag) || etagValue(current.ETag) != tags[0].value {
				return cond, storage.ErrConflict
			}
			cond.IfMatch = current.ETag
		default:
			current, statErr := h.backend.Stat(ctx, key)
			if statErr != nil {
				if errors.Is(statErr, storage.ErrNotFound) {
					return cond, storage.ErrConflict
				}
				return cond, statErr
			}
			currentTag := etagValue(current.ETag)
			matched := false
			for _, tag := range tags {
				if !entityTagIsWeak(current.ETag) && !tag.weak && tag.value == currentTag {
					matched = true
				}
			}
			if !matched || current.ETag == "" {
				return cond, storage.ErrConflict
			}
			cond.IfMatch = current.ETag
		}
	}
	if values := r.Header.Values("If-None-Match"); len(values) > 0 {
		tags, wildcard, err := parseEntityTags(strings.Join(values, ","))
		if err != nil {
			return cond, handlerError{status: http.StatusBadRequest, message: "invalid If-None-Match header"}
		}
		if !wildcard || len(tags) != 0 {
			return cond, handlerError{status: http.StatusNotImplemented, message: "only If-None-Match: * is supported for writes"}
		}
		cond.IfNoneMatch = true
	}
	return cond, nil
}

type handlerError struct {
	status  int
	message string
}

func (e handlerError) Error() string { return e.message }

func writeHandlerError(w http.ResponseWriter, err error) {
	var target handlerError
	if errors.As(err, &target) {
		http.Error(w, target.message, target.status)
		return
	}
	writeStorageError(w, err)
}

func (h *handler) mkcol(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	rel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if rel == "" {
		http.Error(w, "cannot create the gateway root", http.StatusMethodNotAllowed)
		return
	}
	if err := h.backend.Mkdir(r.Context(), h.key(rel)); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	rel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if rel == "" {
		http.Error(w, "cannot delete the gateway root", http.StatusMethodNotAllowed)
		return
	}
	key := h.key(rel)
	entry, err := h.backend.Stat(r.Context(), key)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if entry.IsDir {
		backend, ok := h.backend.(CopyMoveBackend)
		if !ok {
			http.Error(w, "recursive directory deletion is not supported", http.StatusMethodNotAllowed)
			return
		}
		if hasHeader(r, "If-Match") || hasHeader(r, "If-None-Match") {
			http.Error(w, "directory DELETE entity-tag conditions are not supported", http.StatusNotImplemented)
			return
		}
		if err := backend.DeleteTree(r.Context(), key); err != nil {
			writeStorageError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cond, err := h.writeCondition(r.Context(), key, r)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	if cond.IfMatch == "" && !cond.IfNoneMatch {
		// Core DELETE commits only against an explicit strong revision. DAV's
		// unconditional and If-Match:* forms resolve to the current revision,
		// then retain that condition through the actual mutation.
		cond.IfMatch = entry.ETag
		if cond.IfMatch == "" || entityTagIsWeak(cond.IfMatch) {
			http.Error(w, "storage object has no stable ETag for conditional deletion", http.StatusConflict)
			return
		}
	}
	if err := h.backend.Delete(r.Context(), key, cond); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func hasHeader(r *http.Request, name string) bool {
	return len(r.Header.Values(name)) > 0
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrLocked):
		http.Error(w, "resource is locked", 423)
	case errors.Is(err, storage.ErrNotFound):
		http.Error(w, "resource not found", http.StatusNotFound)
	case errors.Is(err, storage.ErrConflict):
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
	case errors.Is(err, storage.ErrInvalidRange):
		http.Error(w, "range is not satisfiable", http.StatusRequestedRangeNotSatisfiable)
	case errors.Is(err, storage.ErrUnsupported):
		http.Error(w, "storage operation is not supported", http.StatusNotImplemented)
	case errors.Is(err, storage.ErrInvalidPath):
		http.Error(w, "invalid path", http.StatusBadRequest)
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		http.Error(w, "incomplete request body", http.StatusBadRequest)
	case errors.Is(err, storage.ErrPartialOperation):
		http.Error(w, "operation partially completed; inspect source and destination before retrying", http.StatusBadGateway)
	default:
		http.Error(w, "storage operation failed", http.StatusBadGateway)
	}
}
