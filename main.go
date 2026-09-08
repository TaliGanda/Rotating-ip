package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	Addr         string    `json:"addr"`
	LatencyMS    int64     `json:"latency_ms"`
	Success      uint64    `json:"success"`
	Failures     uint64    `json:"failures"`
	LastSuccess  time.Time `json:"last_success"`
	LastFailure  time.Time `json:"last_failure"`
	CooldownTill time.Time `json:"cooldown_till"`
	Healthy      bool      `json:"healthy"`
}

type ProxyManager struct {
	mu sync.RWMutex

	proxies map[string]*Proxy

	apiURL          string
	healthURL       string
	refreshInterval time.Duration
	healthTimeout   time.Duration
	apiTimeout      time.Duration
	checkWorkers    int
	maxFailures     int
	cooldownBase    time.Duration

	refreshing atomic.Bool
}

func NewProxyManager() *ProxyManager {
	apiURL := strings.TrimSpace(os.Getenv("PROXY_API_URL"))
	if apiURL == "" {
		log.Fatal("PROXY_API_URL belum di-set")
	}

	return &ProxyManager{
		proxies:         make(map[string]*Proxy),
		apiURL:          apiURL,
		healthURL:       envString("HEALTH_URL", "http://connectivitycheck.gstatic.com/generate_204"),
		refreshInterval: envDuration("REFRESH_INTERVAL", 5*time.Minute),
		healthTimeout:   envDuration("HEALTH_TIMEOUT", 1200*time.Millisecond),
		apiTimeout:      envDuration("API_TIMEOUT", 15*time.Second),
		checkWorkers:    envInt("CHECK_WORKERS", 100),
		maxFailures:     envInt("MAX_FAILURES", 3),
		cooldownBase:    envDuration("COOLDOWN_BASE", 15*time.Second),
	}
}

func envString(name, fallback string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v
}

func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}

	return n
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}

	return d
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))

	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}

		seen[v] = struct{}{}
		out = append(out, v)
	}

	return out
}

func validateHostPort(v string) (string, bool) {
	v = strings.TrimSpace(v)

	// username/password@host:port
	if at := strings.LastIndex(v, "@"); at >= 0 {
		auth := v[:at]
		hostport := v[at+1:]

		hostport, ok := validateHostPort(hostport)
		if !ok || auth == "" {
			return "", false
		}

		return auth + "@" + hostport, true
	}

	host, port, err := net.SplitHostPort(v)
	if err != nil {
		parts := strings.Split(v, ":")
		if len(parts) != 2 {
			return "", false
		}

		host = strings.TrimSpace(parts[0])
		port = strings.TrimSpace(parts[1])
	}

	if host == "" || port == "" {
		return "", false
	}

	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}

	return net.JoinHostPort(host, strconv.Itoa(n)), true
}

func normalizeProxyString(v string) (string, bool) {
	v = strings.TrimSpace(v)

	if v == "" || strings.HasPrefix(v, "#") {
		return "", false
	}

	v = strings.TrimPrefix(v, "http://")
	v = strings.TrimPrefix(v, "https://")

	return validateHostPort(v)
}

func normalizeProxy(item any) (string, bool) {
	switch x := item.(type) {
	case string:
		return normalizeProxyString(x)

	case map[string]any:
		host := ""

		for _, key := range []string{
			"ip",
			"host",
			"hostname",
			"address",
		} {
			if value, ok := x[key]; ok {
				if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
					host = strings.TrimSpace(s)
					break
				}
			}
		}

		if host == "" {
			return "", false
		}

		var port string

		if value, ok := x["port"]; ok {
			switch p := value.(type) {
			case float64:
				port = strconv.Itoa(int(p))
			case string:
				port = strings.TrimSpace(p)
			case int:
				port = strconv.Itoa(p)
			case int64:
				port = strconv.FormatInt(p, 10)
			}
		}

		if port == "" {
			return "", false
		}

		return validateHostPort(host + ":" + port)

	default:
		return "", false
	}
}

func (pm *ProxyManager) fetchProxies(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		pm.apiURL,
		nil,
	)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: pm.apiTimeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf(
			"proxy API HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	body, err := io.ReadAll(
		io.LimitReader(resp.Body, 50<<20),
	)
	if err != nil {
		return nil, err
	}

	if len(body) == 0 {
		return nil, errors.New("API mengembalikan response kosong")
	}

	// JSON
	var raw any

	if err := json.Unmarshal(body, &raw); err == nil {
		var items []any

		switch x := raw.(type) {
		case []any:
			items = x

		case map[string]any:
			for _, key := range []string{
				"data",
				"proxies",
				"results",
			} {
				if arr, ok := x[key].([]any); ok {
					items = arr
					break
				}
			}
		}

		result := make([]string, 0, len(items))

		for _, item := range items {
			if p, ok := normalizeProxy(item); ok {
				result = append(result, p)
			}
		}

		return uniqueStrings(result), nil
	}

	// Plain text
	lines := strings.Split(string(body), "\n")
	result := make([]string, 0, len(lines))

	for _, line := range lines {
		if p, ok := normalizeProxyString(line); ok {
			result = append(result, p)
		}
	}

	return uniqueStrings(result), nil
}

func (pm *ProxyManager) checkProxy(proxyAddr string) (time.Duration, bool) {
	proxyURL, err := parseProxyURL(proxyAddr)
	if err != nil {
		return 0, false
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),

		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     2 * time.Second,

		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   pm.healthTimeout,
		CheckRedirect: func(
			req *http.Request,
			via []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(
		http.MethodGet,
		pm.healthURL,
		nil,
	)
	if err != nil {
		return 0, false
	}

	req.Header.Set(
		"User-Agent",
		"RotatingProxyHealthCheck/1.0",
	)

	start := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}

	defer resp.Body.Close()

	_, _ = io.Copy(
		io.Discard,
		io.LimitReader(resp.Body, 64),
	)

	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusNoContent {
		return elapsed, false
	}

	return elapsed, elapsed <= pm.healthTimeout
}

func parseProxyURL(proxyAddr string) (*url.URL, error) {
	value := proxyAddr

	if !strings.Contains(value, "://") {
		value = "http://" + value
	}

	return url.Parse(value)
}

func splitProxyAuth(proxyAddr string) (
	user string,
	pass string,
	hostport string,
) {
	if at := strings.LastIndex(proxyAddr, "@"); at >= 0 {
		auth := proxyAddr[:at]
		hostport = proxyAddr[at+1:]

		if colon := strings.Index(auth, ":"); colon >= 0 {
			user = auth[:colon]
			pass = auth[colon+1:]
		} else {
			user = auth
		}

		return
	}

	return "", "", proxyAddr
}

func (pm *ProxyManager) testOne(addr string) {
	latency, ok := pm.checkProxy(addr)

	pm.mu.Lock()
	defer pm.mu.Unlock()

	p, exists := pm.proxies[addr]
	if !exists {
		p = &Proxy{
			Addr: addr,
		}
		pm.proxies[addr] = p
	}

	if ok {
		p.Healthy = true
		p.LatencyMS = latency.Milliseconds()

		if p.LatencyMS <= 0 {
			p.LatencyMS = 1
		}

		p.Success++
		p.Failures = 0
		p.LastSuccess = time.Now()
		p.CooldownTill = time.Time{}

		return
	}

	p.Healthy = false
	p.Failures++
	p.LastFailure = time.Now()

	failure := int(p.Failures)
	if failure > pm.maxFailures {
		failure = pm.maxFailures
	}

	backoffMultiplier := 1 << (failure - 1)

	p.CooldownTill = time.Now().Add(
		pm.cooldownBase *
			time.Duration(backoffMultiplier),
	)
}

func (pm *ProxyManager) Refresh(ctx context.Context) error {
	if !pm.refreshing.CompareAndSwap(false, true) {
		return nil
	}

	defer pm.refreshing.Store(false)

	proxies, err := pm.fetchProxies(ctx)
	if err != nil {
		return err
	}

	if len(proxies) == 0 {
		return errors.New("tidak ada proxy valid")
	}

	log.Printf(
		"API mengembalikan %d proxy",
		len(proxies),
	)

	sem := make(chan struct{}, pm.checkWorkers)

	var wg sync.WaitGroup

	for _, addr := range proxies {
		addr := addr

		pm.mu.Lock()

		if _, exists := pm.proxies[addr]; !exists {
			pm.proxies[addr] = &Proxy{
				Addr: addr,
			}
		}

		pm.mu.Unlock()

		wg.Add(1)

		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() {
				<-sem
			}()

			pm.testOne(addr)
		}()
	}

	wg.Wait()

	count := pm.HealthyCount()

	log.Printf(
		"health-check selesai: %d/%d proxy sehat",
		count,
		len(proxies),
	)

	// Simpan hanya proxy sehat
	pm.SaveHealthy("rotating.txt")

	return nil
}

func (pm *ProxyManager) HealthyCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	now := time.Now()
	count := 0

	for _, p := range pm.proxies {
		if !p.Healthy {
			continue
		}

		if !p.CooldownTill.IsZero() &&
			now.Before(p.CooldownTill) {
			continue
		}

		count++
	}

	return count
}

func (pm *ProxyManager) Pick() (*Proxy, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	now := time.Now()

	candidates := make([]*Proxy, 0)

	for _, p := range pm.proxies {
		if !p.Healthy {
			continue
		}

		if !p.CooldownTill.IsZero() &&
			now.Before(p.CooldownTill) {
			continue
		}

		candidates = append(candidates, p)
	}

	if len(candidates) == 0 {
		return nil, errors.New("tidak ada proxy sehat")
	}

	// Proxy tercepat diprioritaskan.
	sort.Slice(
		candidates,
		func(i, j int) bool {
			if candidates[i].LatencyMS !=
				candidates[j].LatencyMS {
				return candidates[i].LatencyMS <
					candidates[j].LatencyMS
			}

			return candidates[i].Success >
				candidates[j].Success
		},
	)

	return candidates[0], nil
}

func (pm *ProxyManager) MarkSuccess(
	addr string,
	latency time.Duration,
) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	p, ok := pm.proxies[addr]
	if !ok {
		return
	}

	p.Healthy = true
	p.Success++
	p.Failures = 0
	p.LastSuccess = time.Now()
	p.CooldownTill = time.Time{}

	if latency > 0 {
		p.LatencyMS = latency.Milliseconds()

		if p.LatencyMS <= 0 {
			p.LatencyMS = 1
		}
	}
}

func (pm *ProxyManager) MarkFailure(addr string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	p, ok := pm.proxies[addr]
	if !ok {
		return
	}

	p.Healthy = false
	p.Failures++
	p.LastFailure = time.Now()

	failure := int(p.Failures)

	if failure > pm.maxFailures {
		failure = pm.maxFailures
	}

	multiplier := 1 << (failure - 1)

	p.CooldownTill = time.Now().Add(
		pm.cooldownBase *
			time.Duration(multiplier),
	)
}

func (pm *ProxyManager) SaveHealthy(filename string) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	f, err := os.Create(filename)
	if err != nil {
		log.Printf(
			"gagal membuat %s: %v",
			filename,
			err,
		)
		return
	}

	defer f.Close()

	now := time.Now()

	for _, p := range pm.proxies {
		if !p.Healthy {
			continue
		}

		if !p.CooldownTill.IsZero() &&
			now.Before(p.CooldownTill) {
			continue
		}

		fmt.Fprintln(f, p.Addr)
	}
}

func (pm *ProxyManager) Snapshot() []Proxy {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make([]Proxy, 0, len(pm.proxies))

	for _, p := range pm.proxies {
		result = append(result, *p)
	}

	sort.Slice(
		result,
		func(i, j int) bool {
			if result[i].Healthy !=
				result[j].Healthy {
				return result[i].Healthy
			}

			return result[i].LatencyMS <
				result[j].LatencyMS
		},
	)

	return result
}

type ProxyServer struct {
	pm *ProxyManager
}

func NewProxyServer(pm *ProxyManager) *ProxyServer {
	return &ProxyServer{
		pm: pm,
	}
}

func (s *ProxyServer) ServeHTTP(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method == http.MethodConnect {
		s.handleCONNECT(w, r)
		return
	}

	s.handleHTTP(w, r)
}

func (s *ProxyServer) handleHTTP(
	w http.ResponseWriter,
	r *http.Request,
) {
	proxy, err := s.pm.Pick()
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusServiceUnavailable,
		)
		return
	}

	proxyURL, err := parseProxyURL(proxy.Addr)
	if err != nil {
		http.Error(
			w,
			"proxy invalid",
			http.StatusBadGateway,
		)
		return
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),

		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,

		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(
			req *http.Request,
			via []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}

	req := r.Clone(r.Context())
	req.RequestURI = ""

	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")

	start := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		s.pm.MarkFailure(proxy.Addr)

		http.Error(
			w,
			fmt.Sprintf(
				"upstream proxy failed: %v",
				err,
			),
			http.StatusBadGateway,
		)

		return
	}

	defer resp.Body.Close()

	s.pm.MarkSuccess(
		proxy.Addr,
		time.Since(start),
	)

	copyHeaders(w.Header(), resp.Header)

	w.WriteHeader(resp.StatusCode)

	_, _ = io.Copy(w, resp.Body)
}

func (s *ProxyServer) handleCONNECT(
	w http.ResponseWriter,
	r *http.Request,
) {
	proxy, err := s.pm.Pick()
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusServiceUnavailable,
		)
		return
	}

	start := time.Now()

	upstream, err := dialProxyCONNECT(
		proxy.Addr,
		r.Host,
	)

	if err != nil {
		s.pm.MarkFailure(proxy.Addr)

		http.Error(
			w,
			fmt.Sprintf(
				"upstream CONNECT failed: %v",
				err,
			),
			http.StatusBadGateway,
		)

		return
	}

	defer upstream.Close()

	clientConn, rw, err := hijackHTTP(w)
	if err != nil {
		return
	}

	defer clientConn.Close()

	_ = rw

	_, _ = clientConn.Write(
		[]byte(
			"HTTP/1.1 200 Connection Established\r\n" +
				"Proxy-Agent: Go-Rotating-Proxy\r\n" +
				"\r\n",
		),
	)

	errCh := make(chan error, 2)

	go func() {
		_, err := io.Copy(upstream, clientConn)
		errCh <- err
	}()

	go func() {
		_, err := io.Copy(clientConn, upstream)
		errCh <- err
	}()

	<-errCh

	s.pm.MarkSuccess(
		proxy.Addr,
		time.Since(start),
	)
}

func dialProxyCONNECT(
	proxyAddr string,
	target string,
) (net.Conn, error) {
	_, pass, hostport := splitProxyAuth(proxyAddr)

	user, _, hostport := splitProxyAuth(proxyAddr)

	conn, err := net.DialTimeout(
		"tcp",
		hostport,
		3*time.Second,
	)

	if err != nil {
		return nil, err
	}

	authHeader := ""

	if user != "" || pass != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(user + ":" + pass),
		)

		authHeader =
			"Proxy-Authorization: Basic " +
				token +
				"\r\n"
	}

	request := fmt.Sprintf(
		"CONNECT %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Connection: Keep-Alive\r\n"+
			"%s\r\n",
		target,
		target,
		authHeader,
	)

	if _, err := conn.Write(
		[]byte(request),
	); err != nil {
		conn.Close()
		return nil, err
	}

	reader := bufio.NewReader(conn)

	resp, err := http.ReadResponse(
		reader,
		&http.Request{
			Method: http.MethodConnect,
		},
	)

	if err != nil {
		conn.Close()
		return nil, err
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()

		return nil, fmt.Errorf(
			"upstream proxy returned %s",
			resp.Status,
		)
	}

	return conn, nil
}

func hijackHTTP(
	w http.ResponseWriter,
) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(
			w,
			"hijacking tidak didukung",
			http.StatusInternalServerError,
		)
		return nil, nil, errors.New(
			"hijacker tidak tersedia",
		)
	}

	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}

	return conn, rw, nil
}

func copyHeaders(
	dst http.Header,
	src http.Header,
) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func healthHandler(
	pm *ProxyManager,
) http.HandlerFunc {
	return func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		items := pm.Snapshot()

		healthy := 0

		for _, p := range items {
			if p.Healthy &&
				(p.CooldownTill.IsZero() ||
					time.Now().After(p.CooldownTill)) {
				healthy++
			}
		}

		response := map[string]any{
			"healthy": healthy,
			"total":   len(items),
		}

		w.Header().Set(
			"Content-Type",
			"application/json",
		)

		_ = json.NewEncoder(w).Encode(response)
	}
}

func proxiesHandler(
	pm *ProxyManager,
) http.HandlerFunc {
	return func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set(
			"Content-Type",
			"application/json",
		)

		_ = json.NewEncoder(w).Encode(
			pm.Snapshot(),
		)
	}
}

func refreshHandler(
	pm *ProxyManager,
) http.HandlerFunc {
	return func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		ctx, cancel := context.WithTimeout(
			r.Context(),
			60*time.Second,
		)
		defer cancel()

		if err := pm.Refresh(ctx); err != nil {
			http.Error(
				w,
				err.Error(),
				http.StatusBadGateway,
			)
			return
		}

		w.Header().Set(
			"Content-Type",
			"application/json",
		)

		_ = json.NewEncoder(w).Encode(
			map[string]any{
				"ok":      true,
				"healthy": pm.HealthyCount(),
			},
		)
	}
}

func periodicRefresh(
	ctx context.Context,
	pm *ProxyManager,
) {
	ticker := time.NewTicker(
		pm.refreshInterval,
	)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(
				ctx,
				60*time.Second,
			)

			err := pm.Refresh(refreshCtx)

			cancel()

			if err != nil {
				log.Printf(
					"refresh gagal: %v",
					err,
				)
			}
		}
	}
}

func main() {
	log.SetFlags(
		log.Ldate |
			log.Ltime |
			log.Lmicroseconds,
	)

	pm := NewProxyManager()

	ctx, cancel := context.WithCancel(
		context.Background(),
	)
	defer cancel()

	// Initial refresh
	initialCtx, initialCancel := context.WithTimeout(
		ctx,
		60*time.Second,
	)

	if err := pm.Refresh(initialCtx); err != nil {
		log.Fatalf(
			"initial refresh gagal: %v",
			err,
		)
	}

	initialCancel()

	// Background refresh
	go periodicRefresh(
		ctx,
		pm,
	)

	// Local proxy
	proxyListen := envString(
		"LISTEN_ADDR",
		"127.0.0.1:8080",
	)

	go func() {
		server := &http.Server{
			Addr:              proxyListen,
			Handler:           NewProxyServer(pm),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		}

		log.Printf(
			"rotating proxy listening on %s",
			proxyListen,
		)

		if err := server.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			log.Fatalf(
				"proxy server error: %v",
				err,
			)
		}
	}()

	// Management API
	managementListen := envString(
		"MANAGEMENT_ADDR",
		"127.0.0.1:8090",
	)

	mux := http.NewServeMux()

	mux.HandleFunc(
		"/health",
		healthHandler(pm),
	)

	mux.HandleFunc(
		"/proxies",
		proxiesHandler(pm),
	)

	mux.HandleFunc(
		"/refresh",
		refreshHandler(pm),
	)

	server := &http.Server{
		Addr:              managementListen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	log.Printf(
		"management API listening on %s",
		managementListen,
	)

	if err := server.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
