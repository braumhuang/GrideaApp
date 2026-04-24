package service

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"gridea-pro/backend/internal/domain"
)

// DefaultDevStartPort 开发模式默认起始端口
const DefaultDevStartPort = 3367

// DefaultProdStartPort 生产模式默认起始端口
const DefaultProdStartPort = 6606

// PreviewService 管理预览服务器的生命周期
type PreviewService struct {
	server    *http.Server
	port      int
	buildDir  string
	mu        sync.RWMutex
	isRunning bool
	logger    *slog.Logger
}

// NewPreviewService 创建新的预览服务实例
func NewPreviewService(buildDir string) *PreviewService {
	return &PreviewService{
		buildDir: buildDir,
		port:     0,
		logger:   slog.Default(),
	}
}

func (s *PreviewService) SetBuildDir(buildDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buildDir = buildDir
}

func (s *PreviewService) IsDevelopmentMode() bool {
	if os.Getenv("devserver") != "" {
		return true
	}
	if os.Getenv("WAILS_DEV") != "" {
		return true
	}
	return false
}

// StartPreviewServer 启动预览服务器
func (s *PreviewService) StartPreviewServer(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isRunning && s.server != nil {
		return fmt.Sprintf("http://127.0.0.1:%d", s.port), nil
	}

	// Determine preferred port
	basePort := DefaultProdStartPort
	if s.IsDevelopmentMode() {
		basePort = DefaultDevStartPort
	}

	// Helper to try listen
	tryListen := func(p int) (net.Listener, error) {
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	}

	// Try ports incrementally
	var listener net.Listener
	var err error
	maxRetries := 20

	for i := 0; i < maxRetries; i++ {
		port := basePort + i
		listener, err = tryListen(port)
		if err == nil {
			break // Successfully bound
		}
		// Only log if it's the specific port we wanted, to avoid spamming logs if we are just scanning
		if i == 0 {
			s.logger.Info("Preview Server: port is in use, attempting to find next available port", "port", port)
		}
	}

	// If scanning fails, fallback to random port
	if err != nil {
		s.logger.Warn("Preview Server: could not find available port in range, falling back to random port", "rangeStart", basePort, "rangeEnd", basePort+maxRetries-1)
		listener, err = tryListen(0)
		if err != nil {
			s.sendToast(ctx, domain.ErrPreviewStartFailed+": "+err.Error(), "error")
			return "", fmt.Errorf(domain.ErrPreviewStartFailed+": %w", err)
		}
	}

	// 2. 获取实际分配的端口
	s.port = listener.Addr().(*net.TCPAddr).Port

	// 3. 配置服务器
	mux := http.NewServeMux()

	// Create a custom handler that falls back to 404.html
	fileServer := http.FileServer(http.Dir(s.buildDir))
	customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.buildDir, filepath.Clean(r.URL.Path))

		// Determine if the file or directory exists
		info, err := os.Stat(path)
		if os.IsNotExist(err) || (info != nil && info.IsDir() && r.URL.Path != "/") {
			// If it's a directory other than root, let's see if index.html exists inside it
			// http.FileServer auto-redirects or serves index.html if present.
			// However, if we just want a simple fallback for completely missing routes:
			if info != nil && info.IsDir() {
				indexPath := filepath.Join(path, "index.html")
				if _, err := os.Stat(indexPath); err == nil {
					fileServer.ServeHTTP(w, r)
					return
				}
			}

			// If file doesn't exist, serve 404.html
			notFoundPath := filepath.Join(s.buildDir, "404.html")
			if content, err := os.ReadFile(notFoundPath); err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write(content)
				return
			}

			// If even 404.html doesn't exist, just let the original fileServer handle (and fail)
		}

		// Serve standard files
		fileServer.ServeHTTP(w, r)
	})

	// 预览专属 kill-switch：拦截 /sw.js 返回一个"自杀"SW。
	//
	// 背景：主题启用 PWA 后，真实的 sw.js 对静态资源走 cache-first；而预览站的
	// CSS/JS 是固定名字（main.min.css 等），没 hash busting。于是切主题后重渲
	// 了新 CSS，浏览器导航仍然从 SW cache 拿到旧 CSS —— 强刷能解除一次但再
	// 导航又坏。HTTP 层的 Cache-Control 对 SW 的 Cache API 完全无效。
	//
	// 预览是开发调试，根本不需要离线 / 缓存。这里永远吐一个立即清 cache +
	// unregister 自己的 SW，让预览彻底没 PWA。生产部署走的是 Gridea Pro 真
	// 生成的 sw.js，不受影响。
	mux.HandleFunc("/sw.js", servePreviewKillSwitchSW)
	mux.Handle("/", noCacheMiddleware(customHandler))

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}

	// 4. 在 goroutine 中启动，使用 Serve(listener) 而不是 ListenAndServe
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("预览服务器错误", "error", err)
		}
	}()

	s.isRunning = true

	// 给一点启动缓冲时间（可选，Server.Serve 已经是即时的了）
	time.Sleep(50 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d", s.port)
	s.logger.Info("预览服务器已启动", "url", url)

	return url, nil
}

// StopPreviewServer 平滑关闭预览服务器
func (s *PreviewService) StopPreviewServer() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.server == nil || !s.isRunning {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.server.Shutdown(ctx); err != nil {
		s.server.Close()
		s.logger.Warn("预览服务器强制关闭", "error", err)
	} else {
		s.logger.Info("预览服务器已平滑关闭")
	}

	s.server = nil
	s.isRunning = false
	s.port = 0

	return nil
}

func (s *PreviewService) GetPreviewURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.port == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

func (s *PreviewService) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isRunning
}

func (s *PreviewService) GetPort() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.port
}

func (s *PreviewService) sendToast(ctx context.Context, message, toastType string) {
	if ctx == nil {
		return
	}
	runtime.EventsEmit(ctx, "app:toast", map[string]interface{}{
		"message":  message,
		"type":     toastType,
		"duration": 3000,
	})
}

// noCacheMiddleware 禁用浏览器缓存，确保主题切换/配置修改后立即加载最新资源
func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

// previewKillSwitchSW 是预览专属的"自杀"Service Worker。
//
// 行为：
//  1. install：跳过 waiting，立即进入 activate
//  2. activate：枚举所有 caches（包括旧版 PWA SW 留下的）全部删掉，
//     然后 unregister 自己；如果确实清掉过东西，再让所有受控页面 navigate 自己
//     触发一次带新资源的重载
//  3. fetch：一律透传给 network，让预览 server 的 no-cache header 决定
//
// 用 `var hadCaches` guard 避免"页面 load → register → navigate → 再 load"
// 的无限循环：只在第一次（caches 里真的有内容）时重载页面，之后 noop。
const previewKillSwitchSW = `// Gridea Pro 预览专属 Service Worker（kill-switch）
// 不要在生产环境使用 —— 本文件只在预览 server 上动态返回。
self.addEventListener('install', function(e) {
  self.skipWaiting();
});

self.addEventListener('activate', function(e) {
  e.waitUntil((async function() {
    var keys = await caches.keys();
    var hadCaches = keys.length > 0;
    await Promise.all(keys.map(function(k) { return caches.delete(k); }));
    try { await self.registration.unregister(); } catch (_) {}
    if (hadCaches) {
      var clients = await self.clients.matchAll({ type: 'window' });
      clients.forEach(function(c) { c.navigate(c.url); });
    }
  })());
});

// 所有请求透传到 network —— 预览 server 自己带 Cache-Control: no-cache
self.addEventListener('fetch', function(e) {
  e.respondWith(fetch(e.request));
});
`

func servePreviewKillSwitchSW(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Service-Worker-Allowed", "/")
	_, _ = w.Write([]byte(previewKillSwitchSW))
}
