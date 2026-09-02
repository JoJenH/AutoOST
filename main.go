package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
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
	configFile  = "config.json"
	storeSearch = "https://store.steampowered.com/api/storesearch/"
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

// searchClient 用于搜索回退的短超时客户端，避免国内超时卡很久。
var searchClient = &http.Client{Timeout: 5 * time.Second}

// steamDir 是用户配置的 Steam 运行目录，下载的 .lua 文件写入这里。
var steamDir string

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

	// 确保已配置 Steam 运行目录（首次启动交互式指定）。
	dir, err := ensureSteamDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	steamDir = dir

	// 确保 OpenSteamTool 已安装（无 .AutoOST.flag 时下载最新发布并解压）。
	if err := ensureTool(steamDir); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	// 单条命令下载指定 id 的 lua：走 CLI，下载完直接退出。
	if len(os.Args) == 2 && isNumeric(os.Args[1]) {
		res, err := download(os.Args[1], "")
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

// ---------------------------------------------------------------- 配置

type config struct {
	SteamDir string `json:"steam_dir"`
}

func loadConfig() (config, error) {
	var cfg config
	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析 %s 失败: %w", configFile, err)
	}
	return cfg, nil
}

func saveConfig(cfg config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configFile, data, 0o644)
}

// ensureSteamDir 返回 Steam 运行目录；首次启动（或目录失效）时交互式指定并保存。
func ensureSteamDir() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if cfg.SteamDir != "" && isDir(cfg.SteamDir) {
		return cfg.SteamDir, nil
	}

	for {
		fmt.Print("请输入 Steam 运行目录: ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return "", fmt.Errorf("读取输入失败")
		}
		dir := expandPath(strings.TrimSpace(scanner.Text()))
		if dir == "" {
			fmt.Println("路径不能为空，请重新输入")
			continue
		}
		if !isDir(dir) {
			fmt.Printf("目录不存在或不是文件夹: %s\n", dir)
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		cfg.SteamDir = abs
		if err := saveConfig(cfg); err != nil {
			return "", err
		}
		return abs, nil
	}
}

// expandPath 展开 ~ 和环境变量（如 $HOME）。
func expandPath(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return os.ExpandEnv(p)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---------------------------------------------------------------- OpenSteamTool 安装

const (
	ostReleasesURL = "https://github.com/OpenSteam001/OpenSteamTool/releases"
	ostFlagName    = ".AutoOST.flag"
)

type ghAsset struct {
	Name               string
	BrowserDownloadURL string
}

// ensureTool 检查 Steam 根目录是否有 .AutoOST.flag，没有则下载最新发布 zip 并解压。
func ensureTool(dir string) error {
	flag := filepath.Join(dir, ostFlagName)
	if _, err := os.Stat(flag); err == nil {
		return nil
	}

	// 解压会覆盖 DLL；Steam 运行时（Windows）会锁住这些文件，需先退出。
	if err := ensureSteamClosed(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "未找到 %s，正在获取 OpenSteamTool 最新发布...\n", ostFlagName)
	asset, err := latestReleaseAsset()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "下载 %s ...\n", asset.Name)

	tmp, err := os.CreateTemp("", "opensteamtool-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := downloadFile(asset.BrowserDownloadURL, tmpPath); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "解压到 %s ...\n", dir)
	if err := extractZip(tmpPath, dir); err != nil {
		return err
	}
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		return err
	}
	return nil
}

// isSteamRunning 检测 Steam 进程是否在运行。仅在 Windows 上检查，
// 因为 Windows 会锁住已加载的 DLL 导致无法覆盖。
func isSteamRunning() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq steam.exe", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "steam.exe")
}

// ensureSteamClosed 若 Steam 在运行则提示用户退出，并等待其关闭。
func ensureSteamClosed() error {
	if !isSteamRunning() {
		return nil
	}
	fmt.Fprintln(os.Stderr, "检测到 Steam 正在运行：OpenSteamTool 解压会覆盖 Steam 已占用的 DLL，请先退出 Steam。")
	for {
		fmt.Print("退出 Steam 后按回车重试（Ctrl-C 取消）: ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return fmt.Errorf("读取输入失败")
		}
		if !isSteamRunning() {
			return nil
		}
		fmt.Println("Steam 仍在运行。")
	}
}

// latestReleaseAsset 不依赖 GitHub API：通过 /releases/latest 跳转拿 tag，
// 再从 /releases/expanded_assets/<tag> 解析 zip 下载链接（避免 API 限流）。
func latestReleaseAsset() (ghAsset, error) {
	tag, err := latestTag()
	if err != nil {
		return ghAsset{}, err
	}
	assets, err := releaseAssets(tag)
	if err != nil {
		return ghAsset{}, err
	}
	return pickZipAsset(assets)
}

func latestTag() (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不跟随跳转，只取 Location
		},
	}
	req, err := http.NewRequest("GET", ostReleasesURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("无法获取 latest 跳转地址 (HTTP %d)", resp.StatusCode)
	}
	tag := path.Base(loc)
	if tag == "" || tag == "." || tag == "/" {
		return "", fmt.Errorf("无法从跳转地址解析 tag: %s", loc)
	}
	return tag, nil
}

func releaseAssets(tag string) ([]ghAsset, error) {
	u := fmt.Sprintf("%s/expanded_assets/%s", ostReleasesURL, tag)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("获取发布资产列表失败 (HTTP %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`releases/download/[^/"]+/([^/"]+\.zip)`)
	var assets []ghAsset
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		filename := m[1]
		assets = append(assets, ghAsset{
			Name:               filename,
			BrowserDownloadURL: fmt.Sprintf("%s/download/%s/%s", ostReleasesURL, tag, filename),
		})
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("发布页里未找到 .zip 资产")
	}
	return assets, nil
}

func pickZipAsset(assets []ghAsset) (ghAsset, error) {
	var zips []ghAsset
	for _, a := range assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
			zips = append(zips, a)
		}
	}
	if len(zips) == 0 {
		return ghAsset{}, fmt.Errorf("最新发布里没有 .zip 资产")
	}
	// 优先非 debug 版本
	for _, a := range zips {
		if !strings.Contains(strings.ToLower(a.Name), "debug") {
			return a, nil
		}
	}
	return zips[0], nil
}

func downloadFile(url, dst string) error {
	req, err := http.NewRequest("GET", url, nil)
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
		return fmt.Errorf("下载失败 (HTTP %d)", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func extractZip(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue // 防 zip-slip
		}
		target := filepath.Join(dest, name)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
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
func download(appid, name string) (downloadResult, error) {
	if out, size, ok := downloadGitHub(appid, name); ok {
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
	out, size, err := downloadLua(appid, token, name)
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

func downloadGitHub(appid, name string) (string, int64, bool) {
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

	basename := luaFilename(appid, name)
	dir, err := luaOutputDir()
	if err != nil {
		return "", 0, false
	}
	size, err := saveFile(filepath.Join(dir, basename), resp.Body)
	if err != nil {
		return "", 0, false
	}
	return basename, size, true
}

func saveFile(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(f, r)
}

// luaOutputDir 返回 .lua 存放目录（Steam 根目录/config/lua），并确保其存在。
func luaOutputDir() (string, error) {
	dir := filepath.Join(steamDir, "config", "lua")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// listLuaFiles 返回 Steam/config/lua 下的 .lua 文件名列表（按名称排序）。
func listLuaFiles() ([]string, error) {
	dir, err := luaOutputDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".lua") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// deleteLuaFile 删除 Steam/config/lua 下的指定 .lua 文件。
func deleteLuaFile(name string) error {
	if name == "" || filepath.Base(name) != name {
		return fmt.Errorf("非法文件名: %s", name)
	}
	dir, err := luaOutputDir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, name))
}

// readLuaFile 读取 Steam/config/lua 下的 .lua 文件内容，按行返回。
func readLuaFile(name string) ([]string, error) {
	if name == "" || filepath.Base(name) != name {
		return nil, fmt.Errorf("非法文件名: %s", name)
	}
	dir, err := luaOutputDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

// luaFilename 生成下载文件名：有名字时为 "名字-appid.lua"，否则 "appid.lua"。
func luaFilename(appid, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return appid + ".lua"
	}
	return sanitizeFilename(name) + "-" + appid + ".lua"
}

// sanitizeFilename 去掉 Windows 非法字符并截断到合理长度。
func sanitizeFilename(name string) string {
	re := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	s := re.ReplaceAllString(name, "_")
	s = strings.TrimRight(s, ". ")
	if s == "" {
		s = "_"
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
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

func downloadLua(appid, token, name string) (string, int64, error) {
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

	basename := luaFilename(appid, name)
	dir, err := luaOutputDir()
	if err != nil {
		return "", 0, err
	}
	size, err := saveFile(filepath.Join(dir, basename), resp.Body)
	if err != nil {
		return "", 0, err
	}
	return basename, size, nil
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
