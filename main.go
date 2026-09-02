package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	baseURL     = "https://walftech.com"
	githubRaw   = "https://github.com/steamtoolsapp/ManifestHub/raw/refs/heads/%s/%s.lua"
	appListURL  = "https://raw.githubusercontent.com/Austrum-lab/steam-appdb/master/data/all.json"
	appListFile = "applist.json"
	storeSearch = "https://store.steampowered.com/api/storesearch/"
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

// searchClient 用于搜索回退的短超时客户端，避免国内超时卡很久。
var searchClient = &http.Client{Timeout: 5 * time.Second}

// ---------------------------------------------------------------- 入口

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintf(os.Stderr, "用法: %s [appid|游戏名|update]\n", os.Args[0])
		os.Exit(1)
	}

	// 手动更新本地游戏列表
	if len(os.Args) == 2 && os.Args[1] == "update" {
		if err := updateAppList(); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		fmt.Println("已更新", appListFile)
		return
	}

	// 单条命令下载指定 id 的 lua：走 CLI，下载完直接退出。
	if len(os.Args) == 2 && isNumeric(os.Args[1]) {
		res, err := download(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		fmt.Printf("[%s] OK -> %s\n", os.Args[1], res.String())
		return
	}

	// 其余情况（无参数 / 游戏名）都进 TUI 搜索并下载。
	apps, err := loadAppList()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	initial := ""
	if len(os.Args) == 2 {
		initial = os.Args[1]
	}
	if err := runApp(initial, apps); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- 本地游戏列表

type appEntry struct {
	ID   int    `json:"appid"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// searchTypes 是纳入本地搜索的类型（其余如 config/tool/video 丢弃）。
var searchTypes = map[string]bool{"game": true, "dlc": true, "music": true}

// loadAppList 读取本地列表；文件不存在时才自动从 GitHub 拉取。
func loadAppList() ([]appEntry, error) {
	if _, err := os.Stat(appListFile); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "未找到 %s，正在从 GitHub 拉取...\n", appListFile)
		if err := fetchAppList(); err != nil {
			return nil, err
		}
	}
	return readAppList(appListFile)
}

// updateAppList 强制重新拉取（手动 update 命令用）。
func updateAppList() error {
	fmt.Fprintf(os.Stderr, "正在从 GitHub 更新 %s ...\n", appListFile)
	return fetchAppList()
}

func fetchAppList() error {
	req, err := http.NewRequest("GET", appListURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("列表下载失败 (HTTP %d)", resp.StatusCode)
	}

	tmp := appListFile + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, appListFile)
}

// readAppList 流式解析 all.json 数组，只保留 searchTypes 里的类型。
func readAppList(path string) ([]appEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil { // 顶层 '['
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	var apps []appEntry
	for dec.More() {
		var e appEntry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		if searchTypes[e.Type] {
			apps = append(apps, e)
		}
	}
	return apps, nil
}

// searchLocal 本地模糊搜索：忽略大小写的子串匹配，精确 > 前缀 > 子串，game 类型优先。
func searchLocal(query string, apps []appEntry) []appEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}

	type scored struct {
		e appEntry
		s int
	}
	var res []scored
	for _, e := range apps {
		idx := strings.Index(strings.ToLower(e.Name), q)
		if idx < 0 {
			continue
		}
		s := 2 // 子串
		if strings.EqualFold(e.Name, q) {
			s = 0 // 精确
		} else if idx == 0 {
			s = 1 // 前缀
		}
		if e.Type != "game" {
			s += 3
		}
		res = append(res, scored{e, s})
	}

	sort.SliceStable(res, func(i, j int) bool {
		if res[i].s != res[j].s {
			return res[i].s < res[j].s
		}
		return res[i].e.ID < res[j].e.ID
	})

	out := make([]appEntry, 0, len(res))
	for _, r := range res {
		out = append(out, r.e)
		if len(out) >= 50 {
			break
		}
	}
	return out
}

// searchRemote 是本地搜索 0 结果时的回退：调 Steam 商店搜索（短超时）。
func searchRemote(name string) ([]appEntry, error) {
	langs := [][2]string{{"english", "US"}, {"schinese", "CN"}}
	if isCJK(name) {
		langs[0], langs[1] = langs[1], langs[0]
	}
	var lastErr error
	for _, l := range langs {
		items, err := searchStore(name, l[0], l[1])
		if err != nil {
			lastErr = err
			continue
		}
		if len(items) > 0 {
			return items, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func searchStore(name, lang, cc string) ([]appEntry, error) {
	req, err := http.NewRequest("GET", storeSearch, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("term", name)
	q.Set("l", lang)
	q.Set("cc", cc)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", userAgent)

	resp, err := searchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("搜索失败 (HTTP %d)", resp.StatusCode)
	}

	var sr struct {
		Items []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}

	var apps []appEntry
	for _, it := range sr.Items {
		if it.Type == "app" {
			apps = append(apps, appEntry{ID: it.ID, Name: it.Name, Type: "game"})
		}
	}
	return apps, nil
}

func isCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x4E00 && r <= 0x9FFF) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 下载编排

type downloadResult struct {
	Source     string // "GitHub" 或 "Walftech"
	File       string
	Size       int64
	Nonce      int64
	PowMs      int64
	Difficulty int
}

func (r downloadResult) String() string {
	if r.Source == "GitHub" {
		return fmt.Sprintf("%s (%s, %d bytes)", r.File, r.Source, r.Size)
	}
	return fmt.Sprintf("%s (%s, %d bytes | PoW %d 次 %dms | difficulty=%d)",
		r.File, r.Source, r.Size, r.Nonce, r.PowMs, r.Difficulty)
}

// download 优先从 GitHub ManifestHub 下载，拿不到再回退 Walftech。
func download(appid string) (downloadResult, error) {
	if out, size, ok := downloadGitHub(appid); ok {
		return downloadResult{Source: "GitHub", File: out, Size: size}, nil
	}

	challenge, difficulty, err := getChallenge()
	if err != nil {
		return downloadResult{}, err
	}
	t0 := time.Now()
	nonce := solvePow(challenge, difficulty)
	powMs := time.Since(t0).Milliseconds()

	token, err := redeem(appid, challenge, nonce)
	if err != nil {
		return downloadResult{}, err
	}
	out, size, err := downloadLua(appid, token)
	if err != nil {
		return downloadResult{}, err
	}
	return downloadResult{
		Source:     "Walftech",
		File:       out,
		Size:       size,
		Nonce:      nonce,
		PowMs:      powMs,
		Difficulty: difficulty,
	}, nil
}

func downloadGitHub(appid string) (string, int64, bool) {
	u := fmt.Sprintf(githubRaw, appid, appid)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", 0, false
	}

	out := appid + ".lua"
	size, err := saveFile(out, resp.Body)
	if err != nil {
		return "", 0, false
	}
	return out, size, true
}

func saveFile(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(f, r)
}

// ---------------------------------------------------------------- Walftech 门禁

// gate 调 /gate.php；429 或响应含 "rápido" 时等 3 秒重试。
func gate(payload map[string]any) (int, []byte, error) {
	const tries = 4
	var status int
	var body []byte

	for i := 0; i < tries; i++ {
		raw, _ := json.Marshal(payload)
		req, err := walftechRequest("POST", "/gate.php", bytes.NewReader(raw), nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			if i == tries-1 {
				return 0, nil, err
			}
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		status, body = resp.StatusCode, data

		if status == 429 || strings.Contains(string(data), "rápido") {
			time.Sleep(3 * time.Second)
			continue
		}
		return status, body, nil
	}
	return status, body, nil
}

func getChallenge() (string, int, error) {
	status, body, err := gate(map[string]any{"action": "challenge"})
	if err != nil {
		return "", 0, err
	}
	var d struct {
		OK         bool   `json:"ok"`
		Challenge  string `json:"challenge"`
		Difficulty int    `json:"difficulty"`
	}
	if json.Unmarshal(body, &d) != nil || !d.OK || d.Challenge == "" {
		return "", 0, fmt.Errorf("challenge 失败 (HTTP %d): %s", status, truncate(body))
	}
	if d.Difficulty == 0 {
		d.Difficulty = 4
	}
	return d.Challenge, d.Difficulty, nil
}

func solvePow(challenge string, difficulty int) int64 {
	target := strings.Repeat("0", difficulty)
	var nonce int64
	for {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", challenge, nonce)))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), target) {
			return nonce
		}
		nonce++
	}
}

func redeem(appid, challenge string, nonce int64) (string, error) {
	status, body, err := gate(map[string]any{
		"action":    "redeem",
		"id":        appid,
		"challenge": challenge,
		"nonce":     strconv.FormatInt(nonce, 10),
	})
	if err != nil {
		return "", err
	}
	var d struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &d) != nil || !d.OK || d.Token == "" {
		return "", fmt.Errorf("redeem 失败 (HTTP %d): %s", status, truncate(body))
	}
	return d.Token, nil
}

func downloadLua(appid, token string) (string, int64, error) {
	q := url.Values{}
	q.Set("id", appid)
	q.Set("token", token)
	q.Set("format", "lua")

	req, err := walftechRequest("GET", "/depotbox_lua.php", nil, q)
	if err != nil {
		return "", 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", 0, fmt.Errorf("下载失败 (HTTP %d): %s", resp.StatusCode, string(data))
	}

	out := appid + ".lua"
	size, err := saveFile(out, resp.Body)
	if err != nil {
		return "", 0, err
	}
	return out, size, nil
}

func walftechRequest(method, path string, body io.Reader, params url.Values) (*http.Request, error) {
	u := baseURL + path
	if params != nil {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", baseURL+"/generator.html")
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	return req, nil
}

// ---------------------------------------------------------------- 工具

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
