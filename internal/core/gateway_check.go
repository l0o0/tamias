package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type GatewayCheckStep struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type GatewayCheckResult struct {
	GatewayID string             `json:"gatewayId"`
	Started   string             `json:"started"`
	Completed string             `json:"completed"`
	ReadOnly  bool               `json:"readOnly"`
	Passed    bool               `json:"passed"`
	Steps     []GatewayCheckStep `json:"steps"`
	Errors    []string           `json:"errors"`
	Residual  []string           `json:"residual"`
}

func (s *Service) gatewayCheckCommand(r *http.Request) (any, error, bool) {
	if r.URL.Path != "/api/gateways/check" {
		return nil, nil, false
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err, true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	out, err := s.CheckGateway(ctx, in.ID)
	return out, err, true
}

// CheckGateway verifies the running, configured HTTP entry itself. It uses
// random names under the gateway root and always attempts cleanup after writes.
func (s *Service) CheckGateway(ctx context.Context, id string) (GatewayCheckResult, error) {
	result := GatewayCheckResult{GatewayID: id, Started: time.Now().UTC().Format(time.RFC3339Nano), Steps: []GatewayCheckStep{}, Errors: []string{}, Residual: []string{}}
	var g Gateway
	var secret string
	var running bool
	s.mu.Lock()
	for _, candidate := range s.cfg.Gateways {
		if candidate.ID == id {
			g = candidate
			break
		}
	}
	if g.ID != "" {
		secret = s.passwords[id]
		running = s.servers[id] != nil && g.Running
	}
	s.mu.Unlock()
	result.ReadOnly = g.ReadOnly
	if g.ID == "" {
		return result, errors.New("找不到网关配置")
	}
	if !running {
		return result, errors.New("请先启动网关再进行自检")
	}
	if secret == "" {
		var err error
		secret, err = s.vault.Get("gateway:" + id)
		if err != nil || secret == "" {
			return result, errors.New("无法读取网关凭据，自检已停止")
		}
	}
	base, err := gatewayCheckURL(g)
	if err != nil {
		return result, err
	}
	client, err := gatewayCheckClient(g)
	if err != nil {
		return result, err
	}
	defer client.CloseIdleConnections()
	baseCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	probe := ".tamiops-check-" + ID()
	sourceName := probe + "-source.txt"
	targetName := probe + "-moved.txt"
	beforeBody, afterBody, readOnlyBody := probe+"-before", probe+"-after", probe+"-readonly"
	potential := []string{}
	probeBodies := map[string]map[string]bool{sourceName: {beforeBody: true, afterBody: true}}
	probeBodies[targetName] = map[string]bool{afterBody: true}
	addPotential := func(name string) {
		for _, old := range potential {
			if old == name {
				return
			}
		}
		potential = append(potential, name)
	}
	check := func(name string, ok bool, detail string) {
		step := GatewayCheckStep{Name: name, Status: "passed"}
		if !ok {
			step.Status, step.Error = "failed", detail
			result.Passed = false
			result.Errors = append(result.Errors, name+"："+detail)
		}
		result.Steps = append(result.Steps, step)
	}
	result.Passed = true
	unauthStatus, _, _, unauthErr := gatewayRequest(baseCtx, client, base, "OPTIONS", "", "", nil, nil)
	check("拒绝未认证请求", unauthErr == nil && unauthStatus == http.StatusUnauthorized, gatewayStatusError(unauthStatus, unauthErr, "未认证请求未返回 401"))
	if unauthErr != nil || unauthStatus != http.StatusUnauthorized {
		appendGatewaySkipped(&result, []string{"认证后入口", "读取权限"}, "认证检查未通过")
		if !g.ReadOnly {
			appendGatewaySkipped(&result, []string{"创建", "读取", "覆盖", "移动", "移动后读取", "删除"}, "认证检查未通过")
		}
	} else {
		status, _, _, reqErr := gatewayRequest(baseCtx, client, base, "OPTIONS", "", g.Username, &secret, nil)
		// A pointer is used only to make the credential path explicit and avoid
		// accidentally serializing it into result values.
		check("认证后入口", reqErr == nil && status == http.StatusOK, gatewayStatusError(status, reqErr, "认证请求未返回 200"))
		if g.ReadOnly {
			listStatus, _, _, listErr := gatewayRequest(baseCtx, client, base, "PROPFIND", "", g.Username, &secret, map[string]string{"Depth": "0"})
			check("只读访问", listErr == nil && listStatus == 207, gatewayStatusError(listStatus, listErr, "只读 PROPFIND 未返回 207"))
			probeURL := gatewayProbeURL(base, sourceName)
			writeStatus, _, _, writeErr := gatewayRequest(baseCtx, client, probeURL, "PUT", readOnlyBody, g.Username, &secret, map[string]string{"If-None-Match": "*"})
			check("拒绝写入", writeErr == nil && writeStatus == http.StatusForbidden, gatewayStatusError(writeStatus, writeErr, "只读入口未以 403 拒绝写入"))
			readStatus, readBody, readHeaders, readErr := gatewayRequest(baseCtx, client, probeURL, "GET", "", g.Username, &secret, nil)
			noResidual := readErr == nil && readStatus == http.StatusNotFound
			cleanupOK := noResidual
			if readErr == nil && readStatus == http.StatusOK && string(readBody) == readOnlyBody && strongTag(readHeaders.Get("ETag")) {
				// If a broken or misconfigured read-only handler accepted the write,
				// ask that same HTTP entry to conditionally remove only our exact probe.
				deleteStatus, _, _, deleteErr := gatewayRequest(baseCtx, client, probeURL, "DELETE", "", g.Username, &secret, map[string]string{"If-Match": readHeaders.Get("ETag")})
				verifyStatus, _, _, verifyErr := gatewayRequest(baseCtx, client, probeURL, "GET", "", g.Username, &secret, nil)
				cleanupOK = deleteErr == nil && deleteStatus == http.StatusNoContent && verifyErr == nil && verifyStatus == http.StatusNotFound
			}
			check("核对只读探针", noResidual || cleanupOK, gatewayStatusError(readStatus, readErr, "只读探针状态不明确"))
			if !cleanupOK {
				result.Residual = append(result.Residual, sourceName)
			}
			check("清理", cleanupOK, "只读入口拒绝写入后探针仍存在或状态不明确")
		} else if reqErr == nil && status == http.StatusOK {
			sourceURL := gatewayProbeURL(base, sourceName)
			targetURL := gatewayProbeURL(base, targetName)
			created := false
			potential = append(potential, sourceName)
			putStatus, _, _, putErr := gatewayRequest(baseCtx, client, sourceURL, "PUT", beforeBody, g.Username, &secret, map[string]string{"If-None-Match": "*"})
			if putErr == nil && putStatus == http.StatusCreated {
				created = true
			}
			check("创建", putErr == nil && putStatus == http.StatusCreated, gatewayStatusError(putStatus, putErr, "探针创建未返回 201"))
			var currentETag string
			if created {
				getStatus, body, headers, getErr := gatewayRequest(baseCtx, client, sourceURL, "GET", "", g.Username, &secret, nil)
				check("读取", getErr == nil && getStatus == http.StatusOK && string(body) == beforeBody, gatewayStatusError(getStatus, getErr, "探针内容与预期不符"))
				currentETag = headers.Get("ETag")
				check("读取版本", strongTag(currentETag), "入口没有返回可靠的 ETag")
			} else {
				appendGatewaySkipped(&result, []string{"读取", "读取版本"}, "创建探针失败")
			}
			if created && strongTag(currentETag) {
				overwriteStatus, _, _, overwriteErr := gatewayRequest(baseCtx, client, sourceURL, "PUT", afterBody, g.Username, &secret, map[string]string{"If-Match": currentETag})
				check("覆盖", overwriteErr == nil && overwriteStatus == http.StatusNoContent, gatewayStatusError(overwriteStatus, overwriteErr, "条件覆盖未返回 204"))
				if overwriteErr == nil && overwriteStatus == http.StatusNoContent {
					getStatus, body, headers, getErr := gatewayRequest(baseCtx, client, sourceURL, "GET", "", g.Username, &secret, nil)
					check("核对覆盖内容", getErr == nil && getStatus == http.StatusOK && string(body) == afterBody, gatewayStatusError(getStatus, getErr, "覆盖后的内容与预期不符"))
					currentETag = headers.Get("ETag")
				} else {
					appendGatewaySkipped(&result, []string{"核对覆盖内容"}, "覆盖失败")
				}
			} else {
				appendGatewaySkipped(&result, []string{"覆盖", "核对覆盖内容"}, "没有可靠的探针版本")
			}
			if created && strongTag(currentETag) {
				moveStatus, _, _, moveErr := gatewayRequest(baseCtx, client, sourceURL, "MOVE", "", g.Username, &secret, map[string]string{"Destination": targetURL, "Overwrite": "F"})
				check("移动", moveErr == nil && (moveStatus == http.StatusCreated || moveStatus == http.StatusNoContent), gatewayStatusError(moveStatus, moveErr, "MOVE 未返回成功状态"))
				if moveErr == nil && (moveStatus == http.StatusCreated || moveStatus == http.StatusNoContent) {
					addPotential(targetName)
					sourceStatus, _, _, sourceErr := gatewayRequest(baseCtx, client, sourceURL, "GET", "", g.Username, &secret, nil)
					check("核对移动源", sourceErr == nil && sourceStatus == http.StatusNotFound, gatewayStatusError(sourceStatus, sourceErr, "移动后源对象仍存在"))
					getStatus, body, _, getErr := gatewayRequest(baseCtx, client, targetURL, "GET", "", g.Username, &secret, nil)
					check("移动后读取", getErr == nil && getStatus == http.StatusOK && string(body) == afterBody, gatewayStatusError(getStatus, getErr, "移动目标内容与预期不符"))
					deleteStatus, _, _, deleteErr := gatewayRequest(baseCtx, client, targetURL, "DELETE", "", g.Username, &secret, nil)
					check("删除", deleteErr == nil && deleteStatus == http.StatusNoContent, gatewayStatusError(deleteStatus, deleteErr, "DELETE 未返回 204"))
					if deleteErr == nil && deleteStatus == http.StatusNoContent {
						verifyStatus, _, _, verifyErr := gatewayRequest(baseCtx, client, targetURL, "GET", "", g.Username, &secret, nil)
						check("核对删除", verifyErr == nil && verifyStatus == http.StatusNotFound, gatewayStatusError(verifyStatus, verifyErr, "探针删除后仍可读取"))
					} else {
						appendGatewaySkipped(&result, []string{"核对删除"}, "删除失败")
					}
				} else {
					appendGatewaySkipped(&result, []string{"核对移动源", "移动后读取", "删除", "核对删除"}, "移动失败")
					targetStatus, _, _, targetErr := gatewayRequest(baseCtx, client, targetURL, "GET", "", g.Username, &secret, nil)
					if targetErr != nil || targetStatus != http.StatusNotFound {
						result.Residual = append(result.Residual, targetName)
						result.Passed = false
					}
				}
			} else {
				appendGatewaySkipped(&result, []string{"移动", "核对移动源", "移动后读取", "删除", "核对删除"}, "没有可移动的探针")
			}
			cleanupErrors := []string{}
			for _, name := range potential {
				probeURL := gatewayProbeURL(base, name)
				status, body, headers, getErr := gatewayRequest(baseCtx, client, probeURL, "GET", "", g.Username, &secret, nil)
				if getErr != nil {
					cleanupErrors = append(cleanupErrors, name+"：无法核对探针")
					result.Residual = append(result.Residual, name)
					continue
				}
				if status == http.StatusNotFound {
					continue
				}
				if status != http.StatusOK || !strongTag(headers.Get("ETag")) || !probeBodies[name][string(body)] {
					cleanupErrors = append(cleanupErrors, name+"：对象内容、状态或 ETag 不明确")
					result.Residual = append(result.Residual, name)
					continue
				}
				deleteStatus, _, _, deleteErr := gatewayRequest(baseCtx, client, probeURL, "DELETE", "", g.Username, &secret, map[string]string{"If-Match": headers.Get("ETag")})
				if deleteErr != nil || deleteStatus != http.StatusNoContent {
					cleanupErrors = append(cleanupErrors, name+"：清理失败")
					result.Residual = append(result.Residual, name)
					continue
				}
				verifyStatus, _, _, verifyErr := gatewayRequest(baseCtx, client, probeURL, "GET", "", g.Username, &secret, nil)
				if verifyErr != nil || verifyStatus != http.StatusNotFound {
					cleanupErrors = append(cleanupErrors, name+"：清理后状态不明确")
					result.Residual = append(result.Residual, name)
				}
			}
			if len(cleanupErrors) == 0 {
				result.Steps = append(result.Steps, GatewayCheckStep{Name: "清理", Status: "passed"})
			} else {
				result.Steps = append(result.Steps, GatewayCheckStep{Name: "清理", Status: "failed", Error: strings.Join(cleanupErrors, "；")})
				result.Errors = append(result.Errors, "清理："+strings.Join(cleanupErrors, "；"))
				result.Passed = false
			}
		}
	}
	if !gatewayCheckHasStep(result.Steps, "清理") {
		if len(result.Residual) == 0 {
			result.Steps = append(result.Steps, GatewayCheckStep{Name: "清理", Status: "passed"})
		} else {
			result.Steps = append(result.Steps, GatewayCheckStep{Name: "清理", Status: "failed", Error: "有探针残留或提交结果不明确"})
			result.Passed = false
		}
	}
	result.Completed = time.Now().UTC().Format(time.RFC3339Nano)
	return result, nil
}

func gatewayCheckURL(g Gateway) (string, error) {
	base := strings.TrimSpace(g.URL)
	if base == "" {
		base = gatewayURL(g)
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("网关地址无效，自检已停止")
	}
	// The configured listener is the only authority check may contact. The
	// server's advertised URL can use a selected LAN address for wildcard binds.
	listenHost := g.ListenHost
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	if g.Port > 0 {
		if port, parseErr := strconv.Atoi(u.Port()); parseErr != nil || port != g.Port {
			return "", errors.New("网关地址与监听端口不匹配，自检已停止")
		}
	}
	if ip := net.ParseIP(listenHost); ip != nil && !ip.IsUnspecified() {
		if !strings.EqualFold(u.Hostname(), listenHost) {
			return "", errors.New("网关地址与监听地址不匹配，自检已停止")
		}
	} else if ip != nil && ip.IsUnspecified() {
		candidate := net.ParseIP(u.Hostname())
		if candidate == nil || !isLocalInterfaceIP(candidate) {
			return "", errors.New("通配监听网关地址不是本机接口，自检已停止")
		}
	} else {
		return "", errors.New("网关监听地址无效，自检已停止")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLocalInterfaceIP(candidate net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		local, _, parseErr := net.ParseCIDR(addr.String())
		if parseErr == nil && local.Equal(candidate) {
			return true
		}
	}
	return false
}

func gatewayProbeURL(base, name string) string {
	u, _ := url.Parse(base)
	u.Path = path.Join(u.Path, url.PathEscape(name))
	return u.String()
}

func gatewayCheckClient(g Gateway) (*http.Client, error) {
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if strings.HasPrefix(gatewayURL(g), "https://") || g.TLSCert != "" {
		certPEM, err := os.ReadFile(g.TLSCert)
		if err != nil {
			return nil, errors.New("无法读取网关证书，自检已停止")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certPEM) {
			return nil, errors.New("网关证书无法作为可信根，自检已停止")
		}
		// Verify hostname remains enabled; trusting the configured certificate
		// does not disable normal certificate name or expiry checks.
		transport.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func gatewayRequest(ctx context.Context, client *http.Client, target, method, body, username string, password *string, headers map[string]string) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return 0, nil, nil, errors.New("无法创建入口请求")
	}
	if password != nil {
		req.SetBasicAuth(username, *password)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if method == "PROPFIND" {
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
		req.Body = io.NopCloser(bytes.NewReader([]byte(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`)))
		req.ContentLength = int64(len(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`))
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return response.StatusCode, nil, response.Header.Clone(), errors.New("读取入口响应失败")
	}
	return response.StatusCode, data, response.Header.Clone(), nil
}

func gatewayStatusError(status int, err error, fallback string) string {
	if err != nil {
		return "入口请求失败：" + safeGatewayRequestError(err)
	}
	if status >= 300 {
		return fmt.Sprintf("%s（HTTP %d）", fallback, status)
	}
	return fallback
}

func safeGatewayRequestError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if strings.Contains(strings.ToLower(uerr.Err.Error()), "certificate") || strings.Contains(strings.ToLower(uerr.Err.Error()), "tls") {
			return "TLS 验证失败"
		}
		return "连接失败或请求被拒绝"
	}
	return "连接失败或请求被拒绝"
}

func appendGatewaySkipped(result *GatewayCheckResult, names []string, reason string) {
	for _, name := range names {
		result.Steps = append(result.Steps, GatewayCheckStep{Name: name, Status: "skipped", Error: reason})
	}
}

func gatewayCheckHasStep(steps []GatewayCheckStep, name string) bool {
	for _, step := range steps {
		if step.Name == name {
			return true
		}
	}
	return false
}
