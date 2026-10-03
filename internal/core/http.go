package core

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"tamiops/internal/gateway"
	"tamiops/internal/storage"
	"time"
)

func (s *Service) Handler(assets http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; object-src 'none'")
			assets.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Header.Get("X-Tami-Client") != "desktop" {
			writeError(w, errors.New("无效的桌面请求"), http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				writeError(w, errors.New("拒绝跨来源请求"), http.StatusForbidden)
				return
			}
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, errors.New("拒绝跨站点请求"), http.StatusForbidden)
			return
		}
		var result any
		var err error
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/api/state":
				result = s.Snapshot()
			case "/api/recovery":
				var entries []RecoveryEntry
				entries, err = s.recoveries()
				result = map[string]any{"entries": entries}
			case "/api/files":
				var entries []storage.Entry
				entries, err = (Backend{s, r.URL.Query().Get("connectionId")}).List(r.Context(), r.URL.Query().Get("path"))
				if entries == nil {
					entries = []storage.Entry{}
				}
				result = map[string]any{"entries": entries}
			default:
				var handled bool
				result, err, handled = s.extendedGet(r)
				if !handled {
					writeError(w, storage.ErrNotFound, 404)
					return
				}
			}
		} else if r.Method == http.MethodPost {
			ctx, finish, taskErr := s.beginTask(r.Context())
			if taskErr != nil {
				writeError(w, taskErr, 503)
				return
			}
			r = r.WithContext(ctx)
			// Complete tracked Core work before writing any response. Some native
			// WebView response writers synchronously dispatch WriteHeader/Write to
			// the UI thread; shutdown runs Close on that thread and waits for tasks.
			// Keeping the response write inside the task would create a wait cycle.
			func() {
				defer finish()
				if r.URL.Path == "/api/files/upload" {
					result, err = (Backend{s, r.URL.Query().Get("connectionId")}).Put(r.Context(), r.URL.Query().Get("path"), r.Body, r.ContentLength, storage.Condition{IfNoneMatch: true})
				} else {
					result, err = s.command(r)
				}
			}()
		} else {
			writeError(w, errors.New("不支持此请求方法"), 405)
			return
		}
		if err != nil {
			code := 400
			if errors.Is(err, storage.ErrNotFound) {
				code = 404
			}
			if errors.Is(err, storage.ErrConflict) {
				code = 409
			}
			writeError(w, err, code)
			return
		}
		if result == nil {
			result = map[string]bool{"ok": true}
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}
func writeError(w http.ResponseWriter, err error, status int) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
func decode(r *http.Request, v any) error {
	limit := int64(64 << 10)
	if r.URL.Path == "/api/config/import" {
		limit = 2 << 20
	}
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		return errors.New("请求内容无效")
	}
	return nil
}

type command struct {
	ID             string `json:"id"`
	Token          string `json:"token"`
	ConnectionID   string `json:"connectionId"`
	Path           string `json:"path"`
	ETag           string `json:"etag"`
	WriteTest      bool   `json:"writeTest"`
	ConfirmDeletes bool   `json:"confirmDeletes"`
	Choice         string `json:"choice"`
}

func (s *Service) command(r *http.Request) (any, error) {
	if v, e, handled := s.connectionEditCommand(r); handled {
		return v, e
	}
	if v, e, handled := s.versionRestoreCommand(r); handled {
		return v, e
	}
	if v, e, handled := s.gatewayCheckCommand(r); handled {
		return v, e
	}
	if v, e, handled := s.backupCleanupCommand(r); handled {
		return v, e
	}
	if v, e, handled := s.extendedCommand(r); handled {
		return v, e
	}
	switch r.URL.Path {
	case "/api/demo":
		id, err := s.Demo()
		return map[string]string{"id": id}, err
	case "/api/pick-folder":
		if s.PickFolder == nil {
			return nil, errors.New("请选择在桌面应用中操作，浏览器开发模式没有原生目录选择器")
		}
		p, err := s.PickFolder()
		return map[string]string{"path": p}, err
	case "/api/connections":
		var in ConnectionInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		return s.AddConnection(ctx, in)
	case "/api/jobs/update":
		var j Job
		if err := decode(r, &j); err != nil {
			return nil, err
		}
		return s.UpdateJob(j)
	case "/api/jobs":
		var j Job
		if err := decode(r, &j); err != nil {
			return nil, err
		}
		return s.AddJob(j)
	case "/api/gateways":
		var g Gateway
		if err := decode(r, &g); err != nil {
			return nil, err
		}
		v, p, err := s.AddGatewayContext(r.Context(), g)
		return map[string]any{"gateway": v, "password": p}, err
	}
	var c command
	if err := decode(r, &c); err != nil {
		return nil, err
	}
	switch r.URL.Path {
	case "/api/connections/test":
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		return s.TestConnection(ctx, c.ID, c.WriteTest)
	case "/api/recovery/save":
		if s.PickSave == nil {
			return nil, errors.New("请在桌面应用中选择恢复副本保存位置")
		}
		dest, err := s.PickSave("恢复副本-" + c.ID)
		if err != nil {
			return nil, err
		}
		if dest == "" {
			return nil, errors.New("已取消保存")
		}
		err = s.saveRecovery(c.ID, dest)
		return map[string]string{"path": dest}, err
	case "/api/connections/delete":
		return nil, s.deleteConnection(c.ID)
	case "/api/files/mkdir":
		return nil, (Backend{s, c.ConnectionID}).Mkdir(r.Context(), c.Path)
	case "/api/files/delete":
		if c.ETag == "" {
			return nil, errors.New("缺少版本信息，请刷新后重试")
		}
		return nil, (Backend{s, c.ConnectionID}).Delete(r.Context(), c.Path, storage.Condition{IfMatch: c.ETag})
	case "/api/files/download":
		if s.PickSave == nil {
			return nil, errors.New("请在桌面应用中选择下载位置")
		}
		dest, err := s.PickSave(path.Base(c.Path))
		if err != nil {
			return nil, err
		}
		if dest == "" {
			return nil, errors.New("已取消下载")
		}
		return s.StartDownload(r.Context(), c.ConnectionID, c.Path, dest)
	case "/api/jobs/preview":
		return s.Preview(r.Context(), c.ID)
	case "/api/jobs/run":
		return s.RunPlan(r.Context(), c.ID, c.Token, c.ConfirmDeletes)
	case "/api/jobs/resolve":
		return s.ResolveConflict(r.Context(), c.ID, c.Path, c.Choice)
	case "/api/jobs/resume":
		return nil, s.ResumeJob(c.ID)
	case "/api/jobs/cancel":
		return nil, s.CancelJob(c.ID)
	case "/api/jobs/retry":
		return s.RetryJob(r.Context(), c.ID)
	case "/api/jobs/pause":
		return nil, s.CancelJob(c.ID)
	case "/api/jobs/delete":
		return nil, s.deleteJob(c.ID)
	case "/api/gateways/start":
		return nil, s.StartGateway(c.ID)
	case "/api/gateways/stop":
		return nil, s.StopGateway(c.ID)
	case "/api/gateways/delete":
		if err := s.StopGateway(c.ID); err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, g := range s.cfg.Gateways {
			if g.ID == c.ID {
				s.cfg.Gateways = append(s.cfg.Gateways[:i], s.cfg.Gateways[i+1:]...)
				delete(s.passwords, c.ID)
				_ = s.vault.Delete("gateway:" + c.ID)
				return nil, s.saveLocked()
			}
		}
		return nil, storage.ErrNotFound
	default:
		return nil, storage.ErrNotFound
	}
}
func (s *Service) deleteConnection(id string) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	var references int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM backup_jobs WHERE connection=? AND status!='deleted')+(SELECT count(*) FROM migration_jobs WHERE deleted=0 AND (source_connection=? OR target_connection=?))`, id, id, id).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return errors.New("请先删除引用此连接的备份或迁移任务")
	}
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM operations WHERE state IN ('committing','uncertain') AND (json_extract(receipt,'$.sourceConnection')=? OR json_extract(receipt,'$.destinationConnection')=?))+(SELECT count(*) FROM downloads WHERE connection=? AND state NOT IN ('done','cancelled'))+(SELECT count(*) FROM cache_entries WHERE connection=?)`, id, id, id, id).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return errors.New("此连接仍有待处理操作、下载或缓存，请先核对并处理")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.cfg.Jobs {
		if j.ConnectionID == id {
			return errors.New("请先删除引用此连接的任务")
		}
	}
	for _, g := range s.cfg.Gateways {
		if g.ConnectionID == id {
			return errors.New("请先删除引用此连接的网关")
		}
	}
	for i, c := range s.cfg.Connections {
		if c.ID == id {
			old := s.cfg.Connections
			s.cfg.Connections = append(append([]Connection{}, old[:i]...), old[i+1:]...)
			if err := s.saveLocked(); err != nil {
				s.cfg.Connections = old
				return err
			}
			delete(s.stores, id)
			if c.Kind != "demo" {
				if err := s.vault.Delete("connection:" + id); err != nil {
					return errors.New("连接已移除，但系统凭据清理失败，请检查钥匙串")
				}
			}
			return nil
		}
	}
	return storage.ErrNotFound
}
func (s *Service) pauseJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == id {
			s.cfg.Jobs[i].Enabled = false
			if s.cfg.Jobs[i].Status != "running" {
				s.cfg.Jobs[i].Status = "paused"
			}
			s.cfg.Jobs[i].Detail = "已暂停，当前提交完成后停止"
			return s.saveLocked()
		}
	}
	return storage.ErrNotFound
}
func (s *Service) deleteJob(id string) error {
	jobLock := s.syncJobLock(id)
	jobLock.Lock()
	defer jobLock.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.cfg.Jobs {
		if j.ID == id {
			if j.Status == "running" {
				return errors.New("任务正在执行，请等待完成")
			}
			s.cfg.Jobs = append(s.cfg.Jobs[:i], s.cfg.Jobs[i+1:]...)
			_, err := s.db.Exec("DELETE FROM baseline WHERE job=?; DELETE FROM sync_dirty WHERE job=?", id, id)
			if err != nil {
				return err
			}
			return s.saveLocked()
		}
	}
	return storage.ErrNotFound
}
func (s *Service) AddGateway(g Gateway) (Gateway, string, error) {
	return s.AddGatewayContext(context.Background(), g)
}
func (s *Service) AddGatewayContext(ctx context.Context, g Gateway) (Gateway, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := s.connection(g.ConnectionID)
	if err != nil {
		return g, "", err
	}
	if c.Kind != "s3" && c.Kind != "demo" {
		return g, "", errors.New("首版网关只导出 S3 和隔离演示空间")
	}
	if err = validKey(g.Prefix); err != nil {
		return g, "", err
	}
	if !g.ReadOnly && !c.Capabilities.ConditionalWrite {
		return g, "", errors.New("请先验证存储条件写入能力")
	}
	if strings.TrimSpace(g.Name) == "" {
		return g, "", errors.New("请填写入口名称")
	}
	if g.Port == 0 {
		g.Port = 19080
	}
	if g.Port < 1024 || g.Port > 65535 {
		return g, "", errors.New("端口范围为 1024–65535")
	}
	if g.Username == "" {
		g.Username = "tamiops"
	}
	if strings.Contains(g.Username, ":") {
		return g, "", errors.New("用户名不能包含冒号")
	}
	st, err := s.store(g.ConnectionID)
	if err != nil {
		return g, "", err
	}
	e, err := st.Stat(ctx, g.Prefix)
	if err != nil {
		return g, "", err
	}
	if !e.IsDir {
		return g, "", errors.New("网关范围必须是已存在的目录")
	}
	if g.ListenHost == "" {
		g.ListenHost = "127.0.0.1"
	}
	ip := net.ParseIP(g.ListenHost)
	if ip == nil {
		return g, "", errors.New("监听地址需要有效 IP")
	}
	if !ip.IsLoopback() {
		if g.TLSCert == "" || g.TLSKey == "" {
			return g, "", errors.New("局域网入口必须配置 HTTPS 证书和私钥路径")
		}
		if _, err = tls.LoadX509KeyPair(g.TLSCert, g.TLSKey); err != nil {
			return g, "", errors.New("无法读取 HTTPS 证书或私钥")
		}
	}
	g.ID = ID()
	g.Running = false
	g.URL = gatewayURL(g)
	secret := ID()
	if c.Kind != "demo" {
		if err = s.vault.Set("gateway:"+g.ID, secret); err != nil {
			return g, "", errors.New("无法保存入口凭据到系统凭据库")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Gateways = append(s.cfg.Gateways, g)
	s.passwords[g.ID] = secret
	if err = s.saveLocked(); err != nil {
		s.cfg.Gateways = s.cfg.Gateways[:len(s.cfg.Gateways)-1]
		delete(s.passwords, g.ID)
		return g, "", err
	}
	return g, secret, nil
}
func (s *Service) StartGateway(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errors.New("应用正在退出")
	}
	if s.servers[id] != nil {
		return nil
	}
	var g *Gateway
	for i := range s.cfg.Gateways {
		if s.cfg.Gateways[i].ID == id {
			g = &s.cfg.Gateways[i]
			break
		}
	}
	if g == nil {
		return storage.ErrNotFound
	}
	if !g.ReadOnly {
		verified := false
		for _, c := range s.cfg.Connections {
			if c.ID == g.ConnectionID {
				verified = c.Tested && c.Capabilities.ConditionalWrite
			}
		}
		if !verified {
			return errors.New("请先重新验证连接的条件写入能力，再启动读写入口")
		}
	}
	password := s.passwords[id]
	if password == "" {
		var err error
		password, err = s.vault.Get("gateway:" + id)
		if err != nil {
			return errors.New("无法读取入口凭据，请重新创建入口")
		}
	}
	host := g.ListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("监听 IP 无效")
	}
	var tlsConfig *tls.Config
	if !ip.IsLoopback() || g.TLSCert != "" || g.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(g.TLSCert, g.TLSKey)
		if err != nil {
			return errors.New("HTTPS 证书或私钥无效，入口未启动")
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(g.Port)))
	if err != nil {
		return errors.New("端口已被占用，请选择其他端口")
	}
	gwHandler := gateway.NewHandler(gateway.Config{Username: g.Username, Password: password, Prefix: g.Prefix, ReadOnly: g.ReadOnly, OnAccess: func(e gateway.AccessEvent) { s.gatewayAccess(id, e) }}, Backend{s, g.ConnectionID})
	if stats, ok := gwHandler.(interface{ Stats() gateway.AccessStats }); ok {
		s.gatewayStats[id] = stats
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	tracked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, finish, err := s.beginTask(r.Context())
		if err != nil {
			http.Error(w, "service stopping", http.StatusServiceUnavailable)
			return
		}
		defer finish()
		gwHandler.ServeHTTP(w, r.WithContext(ctx))
	})
	srv := &http.Server{Handler: tracked, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	s.servers[id] = srv
	g.Running = true
	go func() {
		err := srv.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			s.activity("gateway", "error", "网关监听异常", g.Name)
		}
	}()
	s.activity("gateway", "success", "本机入口已启动", g.Name)
	return nil
}
func (s *Service) StopGateway(id string) error {
	s.mu.Lock()
	srv := s.servers[id]
	delete(s.servers, id)
	found := false
	for i := range s.cfg.Gateways {
		if s.cfg.Gateways[i].ID == id {
			found = true
			s.cfg.Gateways[i].Running = false
		}
	}
	s.mu.Unlock()
	if !found {
		return storage.ErrNotFound
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
	}
	return nil
}

func gatewayURL(g Gateway) string {
	host := g.ListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if g.TLSCert != "" {
		scheme = "https"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, addr := range addrs {
				candidate, _, _ := net.ParseCIDR(addr.String())
				if candidate != nil && !candidate.IsLoopback() && candidate.To4() != nil && candidate.IsPrivate() {
					host = candidate.String()
					break
				}
			}
		}
	}
	return scheme + "://" + net.JoinHostPort(host, fmt.Sprint(g.Port)) + "/"
}
