package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	status int
	ok     bool
	omdbOK bool
	err    string
	lat    time.Duration
	bytes  int64
}

type omdbResp struct {
	Response string `json:"Response"`
	Error    string `json:"Error"`
}

func main() {
	base := flag.String("base", "https://omdbapi.ailinyu.de", "API base URL，例如 https://omdbapi.ailinyu.de")
	key := flag.String("key", os.Getenv("OMDB_CLIENT_KEY"), "客户端 CLIENT_KEY；也可用环境变量 OMDB_CLIENT_KEY")
	total := flag.Int("n", 100, "总请求数")
	concurrency := flag.Int("c", 10, "并发数")
	mode := flag.String("mode", "title", "请求模式：title/search/id/poster/custom")
	query := flag.String("q", "Inception", "title/search 模式的示例标题或关键词")
	imdbID := flag.String("id", "tt1375666", "id/poster 模式的 IMDb ID")
	path := flag.String("path", "/", "custom 模式的路径，例如 /?t=Inception；不要带 apikey")
	plot := flag.String("plot", "short", "title/id 模式的 plot 参数：short/full/空")
	timeout := flag.Duration("timeout", 15*time.Second, "单请求超时")
	headerKey := flag.Bool("header-key", false, "用 X-API-Key 请求头传 key，而不是 URL apikey 参数")
	insecure := flag.Bool("insecure", false, "跳过 TLS 证书校验，仅调试用")
	jsonOut := flag.Bool("json", false, "输出 JSON 汇总")
	flag.Parse()

	if *key == "" {
		fatal("缺少 CLIENT_KEY：请使用 -key YOUR_CLIENT_KEY 或设置环境变量 OMDB_CLIENT_KEY")
	}
	if *total <= 0 || *concurrency <= 0 {
		fatal("-n 和 -c 必须大于 0")
	}
	if *concurrency > *total {
		*concurrency = *total
	}

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          max(100, *concurrency*2),
			MaxIdleConnsPerHost:   max(100, *concurrency*2),
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec
		},
	}

	jobs := make(chan int)
	results := make(chan result, *total)
	var done int64
	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				res := doRequest(client, buildURL(*base, *mode, *path, *query, *imdbID, *plot, *key, *headerKey, idx), *key, *headerKey)
				atomic.AddInt64(&done, 1)
				results <- res
			}
		}()
	}

	go func() {
		for i := 0; i < *total; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	collected := make([]result, 0, *total)
	for r := range results {
		collected = append(collected, r)
		if !*jsonOut && (len(collected)%max(1, *total/10) == 0 || len(collected) == *total) {
			fmt.Fprintf(os.Stderr, "进度：%d/%d\n", len(collected), *total)
		}
	}

	summary := summarize(collected, time.Since(start), *base, *mode, *total, *concurrency)
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(summary)
		return
	}
	printSummary(summary)
}

func doRequest(client *http.Client, target, key string, headerKey bool) result {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return result{err: err.Error()}
	}
	if headerKey {
		req.Header.Set("X-API-Key", key)
	}
	req.Header.Set("Accept", "application/json,*/*;q=0.8")
	t0 := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(t0)
	if err != nil {
		return result{err: err.Error(), lat: lat}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return result{status: resp.StatusCode, err: readErr.Error(), lat: lat}
	}

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	omdbOK := false
	var parsed omdbResp
	if strings.Contains(resp.Header.Get("Content-Type"), "json") && json.Unmarshal(body, &parsed) == nil {
		omdbOK = strings.EqualFold(parsed.Response, "True")
		if strings.EqualFold(parsed.Response, "False") && parsed.Error != "" {
			ok = false
		}
	}
	return result{status: resp.StatusCode, ok: ok, omdbOK: omdbOK, lat: lat, bytes: int64(len(body))}
}

func buildURL(base, mode, customPath, q, id, plot, key string, headerKey bool, idx int) string {
	base = strings.TrimRight(base, "/")
	var u *url.URL
	if mode == "custom" {
		if strings.HasPrefix(customPath, "http://") || strings.HasPrefix(customPath, "https://") {
			u, _ = url.Parse(customPath)
		} else {
			u, _ = url.Parse(base + ensureSlash(customPath))
		}
	} else if mode == "poster" {
		u, _ = url.Parse(base + "/poster")
	} else {
		u, _ = url.Parse(base + "/")
	}
	params := u.Query()
	if !headerKey {
		params.Set("apikey", key)
	}
	switch mode {
	case "title":
		params.Set("t", q)
		if plot != "" {
			params.Set("plot", plot)
		}
	case "search":
		params.Set("s", q)
		params.Set("page", fmt.Sprintf("%d", idx%5+1))
	case "id", "poster":
		params.Set("i", id)
		if mode == "id" && plot != "" {
			params.Set("plot", plot)
		}
	case "custom":
		// 使用调用者自定义参数。
	}
	u.RawQuery = params.Encode()
	return u.String()
}

type summary struct {
	Base        string             `json:"base"`
	Mode        string             `json:"mode"`
	Total       int                `json:"total"`
	Concurrency int                `json:"concurrency"`
	DurationMS  int64              `json:"durationMs"`
	RPS         float64            `json:"rps"`
	OK          int                `json:"ok"`
	OMDBOK      int                `json:"omdbOk"`
	Failed      int                `json:"failed"`
	Bytes       int64              `json:"bytes"`
	LatencyMS   map[string]float64 `json:"latencyMs"`
	Statuses    map[string]int     `json:"statuses"`
	Errors      map[string]int     `json:"errors"`
}

func summarize(results []result, duration time.Duration, base, mode string, total, concurrency int) summary {
	latencies := make([]time.Duration, 0, len(results))
	statuses := map[string]int{}
	errors := map[string]int{}
	var ok, omdbOK int
	var bytes int64
	for _, r := range results {
		if r.status != 0 {
			statuses[fmt.Sprintf("%d", r.status)]++
		}
		if r.err != "" {
			errors[shortErr(r.err)]++
		}
		if r.ok {
			ok++
		}
		if r.omdbOK {
			omdbOK++
		}
		if r.lat > 0 {
			latencies = append(latencies, r.lat)
		}
		bytes += r.bytes
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	lat := map[string]float64{}
	for _, p := range []int{50, 90, 95, 99} {
		lat[fmt.Sprintf("p%d", p)] = ms(percentile(latencies, p))
	}
	lat["avg"] = ms(avg(latencies))
	return summary{
		Base:        base,
		Mode:        mode,
		Total:       total,
		Concurrency: concurrency,
		DurationMS:  duration.Milliseconds(),
		RPS:         float64(len(results)) / duration.Seconds(),
		OK:          ok,
		OMDBOK:      omdbOK,
		Failed:      len(results) - ok,
		Bytes:       bytes,
		LatencyMS:   lat,
		Statuses:    statuses,
		Errors:      errors,
	}
}

func printSummary(s summary) {
	fmt.Println("\n===== 压测结果 =====")
	fmt.Printf("目标：%s  模式：%s\n", s.Base, s.Mode)
	fmt.Printf("请求数：%d  并发：%d  耗时：%.2fs  RPS：%.2f\n", s.Total, s.Concurrency, float64(s.DurationMS)/1000, s.RPS)
	fmt.Printf("HTTP/API 成功：%d  失败：%d  OMDb Response=True：%d\n", s.OK, s.Failed, s.OMDBOK)
	fmt.Printf("延迟 ms：avg=%.1f p50=%.1f p90=%.1f p95=%.1f p99=%.1f\n", s.LatencyMS["avg"], s.LatencyMS["p50"], s.LatencyMS["p90"], s.LatencyMS["p95"], s.LatencyMS["p99"])
	fmt.Printf("状态码：%v\n", s.Statuses)
	if len(s.Errors) > 0 {
		fmt.Printf("错误：%v\n", s.Errors)
	}
}

func percentile(values []time.Duration, p int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	idx := (len(values)*p+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func avg(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	var sum time.Duration
	for _, v := range values {
		sum += v
	}
	return sum / time.Duration(len(values))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func shortErr(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func ensureSlash(s string) string {
	if strings.HasPrefix(s, "/") {
		return s
	}
	return "/" + s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "错误："+msg)
	os.Exit(2)
}
