package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oschwald/geoip2-golang"
)

// --- 配置常量 ---
const (
	UpdateInterval  = 24 * time.Hour // 数据库更新频率
	DownloadTimeout = 5 * time.Minute
)

// 数据库下载地址
var dbUrls = map[string]string{
	"ASN":     "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-ASN.mmdb",
	"City":    "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-City.mmdb",
	"Country": "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-Country.mmdb",
}

// --- GeoIP 服务 ---

type GeoService struct {
	mu      sync.RWMutex
	asn     *geoip2.Reader
	city    *geoip2.Reader
	country *geoip2.Reader
	dataDir string
}

func NewGeoService(dataDir string) *GeoService {
	return &GeoService{
		dataDir: dataDir,
	}
}

// Load 加载本地数据库
func (g *GeoService) Load() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 辅助关闭旧连接
	closeDB := func(r *geoip2.Reader) {
		if r != nil {
			_ = r.Close()
		}
	}

	// 加载 ASN
	if asn, err := geoip2.Open(filepath.Join(g.dataDir, "GeoLite2-ASN.mmdb")); err == nil {
		closeDB(g.asn) // 关闭旧的
		g.asn = asn
		slog.Info("Loaded ASN DB")
	} else {
		slog.Warn("Failed to load ASN DB", "err", err)
	}

	// 加载 City
	if city, err := geoip2.Open(filepath.Join(g.dataDir, "GeoLite2-City.mmdb")); err == nil {
		closeDB(g.city)
		g.city = city
		slog.Info("Loaded City DB")
	} else {
		slog.Warn("Failed to load City DB", "err", err)
	}

	// 加载 Country
	if country, err := geoip2.Open(filepath.Join(g.dataDir, "GeoLite2-Country.mmdb")); err == nil {
		closeDB(g.country)
		g.country = country
		slog.Info("Loaded Country DB")
	} else {
		slog.Warn("Failed to load Country DB", "err", err)
	}

	return nil
}

// Lookup 查询 IP 信息 (线程安全)
func (g *GeoService) Lookup(ip net.IP) map[string]string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := make(map[string]string)

	// City 解析
	if g.city != nil {
		if record, err := g.city.City(ip); err == nil {
			if record.Country.IsoCode != "" {
				result["x-user-country"] = record.Country.IsoCode
			}
			if len(record.Subdivisions) > 0 {
				if name, ok := record.Subdivisions[0].Names["en"]; ok {
					result["x-user-region"] = name
				}
			}
			if name, ok := record.City.Names["en"]; ok {
				result["x-user-city"] = name
			}
			result["x-user-latitude"] = strconv.FormatFloat(record.Location.Latitude, 'f', 6, 64)
			result["x-user-longitude"] = strconv.FormatFloat(record.Location.Longitude, 'f', 6, 64)
			result["x-user-timezone"] = record.Location.TimeZone
			result["x-user-postal-code"] = record.Postal.Code
		}
	}

	// Country 兜底
	if _, ok := result["x-user-country"]; !ok && g.country != nil {
		if record, err := g.country.Country(ip); err == nil {
			result["x-user-country"] = record.Country.IsoCode
		}
	}

	// ASN 解析
	if g.asn != nil {
		if record, err := g.asn.ASN(ip); err == nil {
			result["x-user-asn"] = strconv.FormatUint(uint64(record.AutonomousSystemNumber), 10)
			result["x-user-asorganization"] = record.AutonomousSystemOrganization
		}
	}

	return result
}

// Close 关闭所有数据库连接
func (g *GeoService) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.asn != nil {
		g.asn.Close()
	}
	if g.city != nil {
		g.city.Close()
	}
	if g.country != nil {
		g.country.Close()
	}
}

// StartAutoUpdate 启动后台更新任务
func (g *GeoService) StartAutoUpdate(ctx context.Context) {
	ticker := time.NewTicker(UpdateInterval)
	defer ticker.Stop()

	// 启动时先尝试下载一次（如果文件不存在）
	g.downloadAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Info("Starting scheduled database update")
			if err := g.downloadAll(); err == nil {
				_ = g.Load() // 重新加载
			}
		}
	}
}

func (g *GeoService) downloadAll() error {
	var wg sync.WaitGroup

	// 确保目录存在
	_ = os.MkdirAll(g.dataDir, 0755)

	for name, urlStr := range dbUrls {
		wg.Add(1)
		go func(n, u string) {
			defer wg.Done()
			if err := downloadFile(filepath.Join(g.dataDir, "GeoLite2-"+n+".mmdb"), u); err != nil {
				slog.Error("Failed to download DB", "type", n, "err", err)
			} else {
				slog.Info("Downloaded DB successfully", "type", n)
			}
		}(name, urlStr)
	}
	wg.Wait()
	return nil
}

func downloadFile(filepath string, url string) error {
	client := http.Client{Timeout: DownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// 下载到临时文件，确保原子操作
	tmpPath := filepath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	_, err = io.Copy(out, resp.Body)
	out.Close() // 必须先关闭文件才能重命名
	if err != nil {
		os.Remove(tmpPath)
		return err
	}

	// 替换旧文件
	return os.Rename(tmpPath, filepath)
}

// --- HTTP 代理逻辑 ---

func getClientIP(r *http.Request) string {
	// 生产环境通常位于 LB (Nginx/AWS ELB/Cloudflare) 之后
	// 这里简单取 X-Forwarded-For 的第一个 IP
	// 如果需要更严格的安全控制，需要实现 Trusted Proxies 检查
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return xrip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func main() {
	// 1. 初始化日志
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// 2. 配置加载
	backendURL := os.Getenv("BACKEND_URL")
	if backendURL == "" {
		slog.Error("BACKEND_URL environment variable is required")
		os.Exit(1)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	target, err := url.Parse(backendURL)
	if err != nil {
		slog.Error("Invalid backend URL", "err", err)
		os.Exit(1)
	}

	// 3. 初始化 GeoService
	geoService := NewGeoService("./data") // 数据存储目录

	// 上下文控制用于优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动后台更新协程
	go geoService.StartAutoUpdate(ctx)

	// 初始加载（等待下载完成或加载本地已有文件）
	// 这里稍微等待一下首次下载，或者直接加载本地
	time.Sleep(2 * time.Second)
	_ = geoService.Load()

	// 4. 配置反向代理
	proxy := httputil.NewSingleHostReverseProxy(target)

	// 自定义 Transport 优化连接池
	proxy.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// 错误处理
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("Proxy error", "err", err, "path", r.URL.Path)
		w.WriteHeader(http.StatusBadGateway)
	}

	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)

		clientIPStr := getClientIP(r)

		// 传递给后端的连接 IP
		r.Header.Set("x-cf-connecting-ip", clientIPStr)

		// 解析 IP
		ip := net.ParseIP(clientIPStr)
		if ip != nil {
			meta := geoService.Lookup(ip)
			for k, v := range meta {
				// 为避免 header 注入攻击，可以做一个简单的字符过滤，这里略过
				r.Header.Set(k, v)
			}
		}

		// 标准代理头维护
		if r.Header.Get("X-Forwarded-For") == "" {
			r.Header.Set("X-Forwarded-For", clientIPStr)
		}
		r.Header.Set("X-Real-IP", clientIPStr)
	}

	// 5. 路由设置
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("/", proxy)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// 6. 启动服务器（协程）
	go func() {
		slog.Info("Server starting", "port", port, "backend", backendURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("ListenAndServe failed", "err", err)
			os.Exit(1)
		}
	}()

	// 7. 优雅停机监听
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("Shutting down server...")

	// 停止更新任务
	cancel()

	// 5秒超时关闭 HTTP 服务
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Server forced to shutdown", "err", err)
	}

	// 关闭数据库连接
	geoService.Close()
	slog.Info("Server exited properly")
}
