package main

import (
	"embed"
	"flag"
	"fmt"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"tamiops/internal/core"
	"tamiops/internal/desktop"
	"time"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/assets/app-icon.png
var appIcon []byte

//go:embed build/assets/tray-icon.png
var trayIcon []byte

//go:embed build/assets/tray-tail/*.png
var trayTailAssets embed.FS

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	serve := flag.Bool("serve", false, "Run a loopback browser development host instead of the native window")
	data := flag.String("data-dir", "", "Isolated application data directory")
	version := flag.Bool("version", false, "Print application version and exit")
	flag.Parse()
	if *version {
		fmt.Println(core.Version)
		return nil
	}
	if *data == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		// Keep the existing workspace location across the product rename.
		*data = filepath.Join(base, "Tami")
	}
	absData, err := filepath.Abs(*data)
	if err != nil {
		return err
	}
	if *serve {
		return runBrowser(absData)
	}
	return runDesktop(absData)
}

func runBrowser(dataDir string) error {
	service, err := core.New(dataDir, nil)
	if err != nil {
		return err
	}
	defer service.Close()
	handler := service.Handler(http.FileServer(http.Dir("frontend/dist")))
	service.StartScheduler()
	service.StartMaintenance()
	localHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:9240" && r.Host != "localhost:9240" {
			http.Error(w, "invalid local host", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: "127.0.0.1:9240", Handler: localHandler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	closed := make(chan struct{})
	go func() { <-sig; _ = srv.Close(); _ = service.Close(); close(closed) }()
	log.Println("Tamias development host: http://127.0.0.1:9240")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-closed
	return nil
}

type handlerSlot struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (s *handlerSlot) Set(handler http.Handler) {
	s.mu.Lock()
	s.handler = handler
	s.mu.Unlock()
}

func (s *handlerSlot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()
	if handler == nil {
		http.Error(w, "application is starting", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

func runDesktop(dataDir string) error {
	configureDesktopIdentity()
	sub, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		return err
	}
	frameEntries, err := trayTailAssets.ReadDir("build/assets/tray-tail")
	if err != nil {
		return err
	}
	trayFrames := make([][]byte, 0, len(frameEntries))
	for _, entry := range frameEntries {
		frame, err := trayTailAssets.ReadFile("build/assets/tray-tail/" + entry.Name())
		if err != nil {
			return err
		}
		trayFrames = append(trayFrames, frame)
	}
	assetHandler := &handlerSlot{}
	notifier := notifications.New()
	var service *core.Service
	var window *application.WebviewWindow
	var windowFocusMu sync.Mutex
	var focusWindowSlot *application.WebviewWindow
	applicationReady := false
	pendingWindowFocus := false
	shuttingDown := false
	focusWindow := func() {
		windowFocusMu.Lock()
		if shuttingDown {
			windowFocusMu.Unlock()
			return
		}
		focusTarget := focusWindowSlot
		if !applicationReady || focusTarget == nil {
			pendingWindowFocus = true
			windowFocusMu.Unlock()
			return
		}
		windowFocusMu.Unlock()
		focusTarget.Show()
		focusTarget.Focus()
	}
	app := application.New(application.Options{Name: "小花鼠", Description: "WebDAV 与 S3 私有存储工作台", Icon: appIcon, LogLevel: slog.LevelWarn,
		Services: []application.Service{application.NewService(notifier)},
		Assets:   application.AssetOptions{Handler: assetHandler, DisableLogging: true},
		Mac:      application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		// Cinnamon uses WM_CLASS as the taskbar name for an AppImage launched
		// without a desktop entry. Keep that name readable on X11 / XWayland.
		Linux: application.LinuxOptions{ApplicationID: "io.tamiops.tami", ProgramName: "Tamias"},
		OnShutdown: func() {
			windowFocusMu.Lock()
			shuttingDown = true
			pendingWindowFocus = false
			windowFocusMu.Unlock()
			if service != nil {
				_ = service.Close()
			}
		},
		SingleInstance: &application.SingleInstanceOptions{UniqueID: "io.tamiops.tami", OnSecondInstanceLaunch: func(application.SecondInstanceData) { focusWindow() }},
	})

	service, err = core.New(dataDir, nil)
	if err != nil {
		return err
	}
	defer service.Close()
	assetHandler.Set(service.Handler(http.FileServerFS(sub)))
	service.SetAutoStart = func(enabled bool) error { return desktop.SetAutoStart(enabled, dataDir) }
	// Refresh the registered executable after an app move or product rename.
	if service.Preferences().AutoStart {
		if err := service.SetAutoStart(true); err != nil {
			log.Printf("更新小花鼠登录启动路径失败：%v", err)
		}
	}
	service.OpenLocal = desktop.Open
	window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "小花鼠", Width: 760, Height: 620, MinWidth: 620, MinHeight: 500,
		URL: "/", BackgroundColour: application.NewRGB(244, 244, 247),
		Mac: application.MacWindow{TitleBar: application.MacTitleBarHiddenInset},
	})
	windowFocusMu.Lock()
	focusWindowSlot = window
	flushPendingFocus := applicationReady && pendingWindowFocus && !shuttingDown
	if flushPendingFocus {
		pendingWindowFocus = false
	}
	windowFocusMu.Unlock()
	if flushPendingFocus {
		window.Show()
		window.Focus()
	}
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		windowFocusMu.Lock()
		applicationReady = true
		flushPendingFocus := pendingWindowFocus && focusWindowSlot != nil && !shuttingDown
		if flushPendingFocus {
			pendingWindowFocus = false
		}
		focusTarget := focusWindowSlot
		windowFocusMu.Unlock()
		if flushPendingFocus {
			focusTarget.Show()
			focusTarget.Focus()
		}
	})
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) { window.Hide(); e.Cancel() })
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { focusWindow() })
	app.Event.OnApplicationEvent(events.Common.SystemDidWake, func(*application.ApplicationEvent) { service.WakeScheduler() })
	app.Event.OnApplicationEvent(events.Mac.ApplicationDidWake, func(*application.ApplicationEvent) { service.WakeScheduler() })
	service.RequestNotifications = desktop.NewAuthorizationRequestWithContext(app.Context(), notifier.RequestNotificationAuthorization, 15*time.Second)
	service.Notify = func(title, body string) {
		if service.Preferences().Notifications {
			allowed, err := notifier.CheckNotificationAuthorization()
			if err == nil && allowed {
				_ = notifier.SendNotification(notifications.NotificationOptions{ID: core.ID(), Title: title, Body: body})
			}
		}
	}
	service.StartScheduler()
	service.StartMaintenance()
	tray := app.SystemTray.New()
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(trayIcon)
	} else {
		tray.SetIcon(trayIcon)
	}
	trayContext := app.Context()
	playTrayGreeting := desktop.NewTrayAnimation(trayContext, trayFrames, trayIcon, 1050*time.Millisecond/time.Duration(len(trayFrames)), func(icon []byte) {
		applied := make(chan struct{})
		application.InvokeAsync(func() {
			defer close(applied)
			// Check on the UI thread as well: a frame queued just before
			// shutdown must never update the destroyed native tray.
			if trayContext.Err() != nil {
				return
			}
			if runtime.GOOS == "darwin" {
				tray.SetTemplateIcon(icon)
			} else {
				tray.SetIcon(icon)
			}
		})
		// Wait for this frame before requesting another, but never wait on
		// the UI during shutdown. This prevents queued frames bunching up.
		select {
		case <-applied:
		case <-trayContext.Done():
		}
	})
	menu := app.NewMenu()
	menu.Add("打开小花鼠").OnClick(func(*application.Context) { window.Show(); window.Focus() })
	menu.Add("暂停所有同步").OnClick(func(*application.Context) {
		for _, job := range service.Snapshot()["jobs"].([]core.Job) {
			_ = service.CancelJob(job.ID)
		}
	})
	menu.Add("恢复所有同步").OnClick(func(*application.Context) {
		for _, job := range service.Snapshot()["jobs"].([]core.Job) {
			_ = service.ResumeJob(job.ID)
		}
		service.WakeScheduler()
	})
	menu.Add("退出小花鼠").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(func() {
		playTrayGreeting()
		focusWindow()
	})
	service.PickFolder = func() (string, error) {
		return desktop.PromptWithContext(app.Context(), func() (string, error) {
			return app.Dialog.OpenFile().SetTitle("选择本地同步文件夹").CanChooseFiles(false).CanChooseDirectories(true).PromptForSingleSelection()
		})
	}
	service.PickSave = func(name string) (string, error) {
		return desktop.PromptWithContext(app.Context(), func() (string, error) {
			dialog := app.Dialog.SaveFile()
			dialog.SetOptions(&application.SaveFileDialogOptions{Title: "保存文件（请选择新文件名）", Filename: strings.TrimSpace(filepath.Base(name)), CanCreateDirectories: true})
			return dialog.PromptForSingleSelection()
		})
	}
	if err = app.Run(); err != nil {
		return fmt.Errorf("桌面应用启动失败：%w", err)
	}
	return nil
}
