package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tamiops/internal/storage"
)

const maxDAVLockRequestBytes = 32 << 10

type davLockScope struct {
	Exclusive *struct{} `xml:"exclusive,omitempty"`
	Shared    *struct{} `xml:"shared,omitempty"`
}

type davLockType struct {
	Write *struct{} `xml:"write,omitempty"`
}

type davLockInfoXML struct {
	XMLName xml.Name     `xml:"lockinfo"`
	Scope   davLockScope `xml:"lockscope"`
	Type    davLockType  `xml:"locktype"`
	Owner   string       `xml:"owner"`
}

type davSupportedLock struct {
	Entries []davLockEntry `xml:"lockentry"`
}

type davLockEntry struct {
	Scope davLockScope `xml:"lockscope"`
	Type  davLockType  `xml:"locktype"`
}

type davLockDiscovery struct {
	Locks []davActiveLock `xml:"activelock,omitempty"`
}

type davActiveLock struct {
	Type    davLockType  `xml:"locktype"`
	Scope   davLockScope `xml:"lockscope"`
	Depth   string       `xml:"depth"`
	Owner   string       `xml:"owner,omitempty"`
	Timeout string       `xml:"timeout"`
	Token   davHref      `xml:"locktoken"`
	Root    davHref      `xml:"lockroot"`
}

type davHref struct {
	Href string `xml:"href"`
}

type davLockResponse struct {
	XMLName       xml.Name         `xml:"prop"`
	XMLNS         string           `xml:"xmlns,attr"`
	LockDiscovery davLockDiscovery `xml:"lockdiscovery"`
}

func parseSimpleIf(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, errors.New("multiple If conditions are not supported")
	}
	value := strings.TrimSpace(values[0])
	if len(value) < 5 || value[0] != '(' || value[len(value)-1] != ')' {
		return nil, errors.New("tagged or compound If condition")
	}
	inside := strings.TrimSpace(value[1 : len(value)-1])
	if len(inside) < 3 || inside[0] != '<' || inside[len(inside)-1] != '>' {
		return nil, errors.New("If condition must contain one lock token")
	}
	token := inside[1 : len(inside)-1]
	if !validLockToken(token) {
		return nil, errors.New("invalid lock token")
	}
	return []string{token}, nil
}

func validLockToken(value string) bool {
	if value == "" || strings.ContainsAny(value, "<> \t\r\n\x00") {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func parseLockTokenHeader(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) >= 3 && value[0] == '<' && value[len(value)-1] == '>' {
		value = value[1 : len(value)-1]
	}
	return value, validLockToken(value)
}

func (h *handler) lock(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	backend, ok := h.backend.(DAVLockBackend)
	if !ok {
		http.Error(w, "WebDAV locks are not supported", http.StatusNotImplemented)
		return
	}
	rel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	key := h.key(rel)
	timeout, err := parseDAVTimeout(r.Header.Values("Timeout"))
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	lockInfo, hasBody, err := readLockInfo(r.Body)
	if err != nil {
		http.Error(w, "invalid LOCK request body", http.StatusBadRequest)
		return
	}
	tokens := storage.LockTokens(r.Context())
	var token string
	var expires time.Time
	createdLockNull := false
	depthHeader := strings.TrimSpace(r.Header.Get("Depth"))
	if depthHeader == "" {
		depthHeader = "infinity"
	}
	if depthHeader != "0" && !strings.EqualFold(depthHeader, "infinity") {
		http.Error(w, "LOCK Depth must be 0 or infinity", http.StatusBadRequest)
		return
	}
	if hasBody {
		if lockInfo.Type.Write == nil || lockInfo.Scope.Exclusive == nil || lockInfo.Scope.Shared != nil {
			http.Error(w, "only exclusive write locks are supported", http.StatusNotImplemented)
			return
		}
		_, statErr := h.backend.Stat(r.Context(), key)
		if errors.Is(statErr, storage.ErrNotFound) {
			if rel == "" {
				http.Error(w, "gateway root cannot be a lock-null resource", http.StatusConflict)
				return
			}
			parent := parentStorageKey(key)
			parentEntry, parentErr := h.backend.Stat(r.Context(), parent)
			if parentErr != nil {
				writeStorageError(w, parentErr)
				return
			}
			if !parentEntry.IsDir {
				http.Error(w, "lock-null resource requires an existing parent collection", http.StatusConflict)
				return
			}
			createdLockNull = true
		} else if statErr != nil {
			writeStorageError(w, statErr)
			return
		}
		token, expires, err = backend.AcquireDAVLock(r.Context(), key, lockInfo.Owner, strings.EqualFold(depthHeader, "infinity"), timeout)
	} else {
		if len(tokens) != 1 {
			http.Error(w, "LOCK refresh requires one simple If lock token", http.StatusBadRequest)
			return
		}
		token = tokens[0]
		expires, err = backend.RefreshDAVLock(r.Context(), key, token, timeout)
	}
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if !validLockToken(token) {
		http.Error(w, "Core returned an invalid lock token", http.StatusBadGateway)
		return
	}
	locks, listErr := backend.DAVLocks(r.Context(), key)
	if listErr != nil {
		writeStorageError(w, listErr)
		return
	}
	response := davLockResponse{XMLNS: "DAV:"}
	for _, active := range locks {
		response.LockDiscovery.Locks = append(response.LockDiscovery.Locks, h.toActiveLock(active, rel))
	}
	w.Header().Set("Lock-Token", "<"+token+">")
	w.Header().Set("Timeout", timeoutHeader(expires))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if createdLockNull {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = xml.NewEncoder(w).Encode(response)
}

func (h *handler) unlock(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	backend, ok := h.backend.(DAVLockBackend)
	if !ok {
		http.Error(w, "WebDAV locks are not supported", http.StatusNotImplemented)
		return
	}
	rel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	values := r.Header.Values("Lock-Token")
	if len(values) != 1 {
		http.Error(w, "UNLOCK requires one Lock-Token", http.StatusBadRequest)
		return
	}
	token, ok := parseLockTokenHeader(values[0])
	if !ok {
		http.Error(w, "invalid Lock-Token", http.StatusBadRequest)
		return
	}
	if err := backend.UnlockDAVLock(r.Context(), h.key(rel), token); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readLockInfo(body io.Reader) (davLockInfoXML, bool, error) {
	if body == nil {
		return davLockInfoXML{}, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, maxDAVLockRequestBytes+1))
	if err != nil {
		return davLockInfoXML{}, false, err
	}
	if len(data) == 0 {
		return davLockInfoXML{}, false, nil
	}
	if len(data) > maxDAVLockRequestBytes {
		return davLockInfoXML{}, false, fmt.Errorf("LOCK body exceeds limit")
	}
	var info davLockInfoXML
	if err := xml.Unmarshal(data, &info); err != nil {
		return davLockInfoXML{}, false, err
	}
	if info.XMLName.Local != "lockinfo" || info.XMLName.Space != "DAV:" {
		return davLockInfoXML{}, false, fmt.Errorf("expected DAV lockinfo")
	}
	return info, true, nil
}

func parseDAVTimeout(values []string) (time.Duration, error) {
	if len(values) == 0 {
		return maxDAVLockTimeout, nil
	}
	for _, value := range values {
		for _, alternative := range strings.Split(value, ",") {
			alternative = strings.TrimSpace(alternative)
			if strings.EqualFold(alternative, "Infinite") {
				continue
			}
			if !strings.HasPrefix(strings.ToLower(alternative), "second-") {
				continue
			}
			seconds, err := strconv.ParseInt(alternative[len("Second-"):], 10, 64)
			if err != nil || seconds <= 0 {
				continue
			}
			if seconds > int64(maxDAVLockTimeout/time.Second) {
				seconds = int64(maxDAVLockTimeout / time.Second)
			}
			return time.Duration(seconds) * time.Second, nil
		}
	}
	return 0, handlerError{status: http.StatusNotImplemented, message: "requested lock timeout is not supported"}
}

func timeoutHeader(expires time.Time) string {
	seconds := int64(time.Until(expires) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return "Second-" + strconv.FormatInt(seconds, 10)
}

func parentStorageKey(key string) string {
	index := strings.LastIndexByte(key, '/')
	if index < 0 {
		return ""
	}
	return key[:index]
}

func baseNameKey(key string) string {
	if index := strings.LastIndexByte(key, '/'); index >= 0 {
		return key[index+1:]
	}
	return key
}

func (h *handler) toActiveLock(lock storage.DAVLock, fallbackRoot string) davActiveLock {
	depth := "0"
	if lock.DepthInfinity {
		depth = "infinity"
	}
	root := lock.RootKey
	if root == "" {
		root = fallbackRoot
	} else if h.prefix != "" {
		switch {
		case root == h.prefix:
			root = ""
		case strings.HasPrefix(root, h.prefix+"/"):
			root = strings.TrimPrefix(root, h.prefix+"/")
		default:
			root = fallbackRoot
		}
	}
	rootIsCollection := lock.DepthInfinity
	active := davActiveLock{
		Type:    davLockType{Write: &struct{}{}},
		Scope:   davLockScope{Exclusive: &struct{}{}},
		Depth:   depth,
		Owner:   lock.Owner,
		Timeout: timeoutHeader(lock.Expires),
		Token:   davHref{Href: lock.Token},
		Root:    davHref{Href: hrefFor(root, rootIsCollection)},
	}
	return active
}

func requestsLockProperties(body io.Reader) (bool, error) {
	if body == nil {
		return true, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, maxDAVLockRequestBytes+1))
	if err != nil {
		return false, err
	}
	if len(data) > maxDAVLockRequestBytes {
		return false, fmt.Errorf("PROPFIND body exceeds limit")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return true, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	stack := make([]xml.Name, 0, 8)
	rootSeen := false
	include := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				rootSeen = value.Name.Space == "DAV:" && value.Name.Local == "propfind"
			}
			if value.Name.Space == "DAV:" && (value.Name.Local == "allprop" || value.Name.Local == "propname") {
				include = true
			}
			if len(stack) > 0 && stack[len(stack)-1].Space == "DAV:" && stack[len(stack)-1].Local == "prop" && value.Name.Space == "DAV:" && (value.Name.Local == "lockdiscovery" || value.Name.Local == "supportedlock") {
				include = true
			}
			stack = append(stack, value.Name)
		case xml.EndElement:
			if len(stack) == 0 {
				return false, fmt.Errorf("unexpected XML closing tag")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if !rootSeen {
		return false, fmt.Errorf("expected DAV propfind")
	}
	return include, nil
}

func (h *handler) addLockProperties(ctx context.Context, key, rel string, response *davResponse) error {
	backend, ok := h.backend.(DAVLockBackend)
	if !ok {
		return nil
	}
	properties := davProperties{
		SupportedLock: &davSupportedLock{Entries: []davLockEntry{{Scope: davLockScope{Exclusive: &struct{}{}}, Type: davLockType{Write: &struct{}{}}}}},
		LockDiscovery: &davLockDiscovery{},
	}
	locks, err := backend.DAVLocks(ctx, key)
	if err != nil {
		return err
	}
	for _, lock := range locks {
		properties.LockDiscovery.Locks = append(properties.LockDiscovery.Locks, h.toActiveLock(lock, rel))
	}
	response.Propstat.Prop.SupportedLock = properties.SupportedLock
	response.Propstat.Prop.LockDiscovery = properties.LockDiscovery
	return nil
}

func (h *handler) copyOrMove(w http.ResponseWriter, r *http.Request, move bool) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	backend, ok := h.backend.(CopyMoveBackend)
	if !ok {
		http.Error(w, "COPY and MOVE are not supported", http.StatusNotImplemented)
		return
	}
	if hasHeader(r, "If-Match") || hasHeader(r, "If-None-Match") {
		http.Error(w, "COPY and MOVE entity-tag conditions are not supported", http.StatusNotImplemented)
		return
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "COPY and MOVE request bodies are not supported", http.StatusUnsupportedMediaType)
		return
	}
	sourceRel, _, err := parseRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if sourceRel == "" {
		http.Error(w, "gateway root cannot be copied or moved", http.StatusMethodNotAllowed)
		return
	}
	destinationRel, err := destinationRelativePath(r)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if destinationRel == "" {
		http.Error(w, "gateway root cannot be overwritten", http.StatusForbidden)
		return
	}
	overwrite := true
	if values := r.Header.Values("Overwrite"); len(values) > 0 {
		if len(values) != 1 || (values[0] != "T" && values[0] != "F") {
			http.Error(w, "Overwrite must be T or F", http.StatusBadRequest)
			return
		}
		overwrite = values[0] == "T"
	}
	if !move {
		depth := strings.TrimSpace(r.Header.Get("Depth"))
		if depth != "" && depth != "0" && depth != "infinity" {
			http.Error(w, "COPY Depth must be 0 or infinity", http.StatusBadRequest)
			return
		}
		if depth == "0" {
			source, statErr := h.backend.Stat(r.Context(), h.key(sourceRel))
			if statErr != nil {
				writeStorageError(w, statErr)
				return
			}
			if source.IsDir {
				http.Error(w, "Depth 0 directory COPY is not supported", http.StatusNotImplemented)
				return
			}
		}
	}
	destinationKey := h.key(destinationRel)
	_, destErr := h.backend.Stat(r.Context(), destinationKey)
	destinationExists := destErr == nil
	if destErr != nil && !errors.Is(destErr, storage.ErrNotFound) {
		writeStorageError(w, destErr)
		return
	}
	if !overwrite && destinationExists {
		http.Error(w, "destination exists and Overwrite is F", http.StatusPreconditionFailed)
		return
	}
	var entry storage.Entry
	if move {
		entry, err = backend.Move(r.Context(), h.key(sourceRel), destinationKey, overwrite)
	} else {
		entry, err = backend.Copy(r.Context(), h.key(sourceRel), destinationKey, overwrite)
	}
	if err != nil {
		if move && errors.Is(err, storage.ErrPartialOperation) {
			markAccessPartial(w)
			writePartialMove(w, sourceRel, destinationRel)
			return
		}
		writeStorageError(w, err)
		return
	}
	_ = entry
	if destinationExists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func destinationRelativePath(r *http.Request) (string, error) {
	values := r.Header.Values("Destination")
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", handlerError{status: http.StatusBadRequest, message: "Destination header is required"}
	}
	value := strings.TrimSpace(values[0])
	destination, err := url.Parse(value)
	if err != nil || destination.User != nil || destination.RawQuery != "" || destination.Fragment != "" || destination.Opaque != "" {
		return "", storage.ErrInvalidPath
	}
	if destination.IsAbs() || destination.Host != "" {
		if destination.Host == "" || !strings.EqualFold(destination.Host, r.Host) {
			return "", storage.ErrInvalidPath
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if !strings.EqualFold(destination.Scheme, scheme) {
			return "", storage.ErrInvalidPath
		}
	}
	rel, _, err := parseEscapedPath(destination.EscapedPath())
	return rel, err
}

func writePartialMove(w http.ResponseWriter, _, _ string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, "MOVE may be partially completed; inspect source and destination before retrying\n")
}

func (h *handler) propPatch(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ReadOnly {
		http.Error(w, "gateway is read-only", http.StatusForbidden)
		return
	}
	http.Error(w, "PROPPATCH properties are not supported by this storage mapping", http.StatusNotImplemented)
}
