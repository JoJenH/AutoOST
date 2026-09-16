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
	"log"
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
	manifestRaw = "https://github.com/steamtoolsapp/ManifestHub/raw/refs/heads/%s/%s"
	appListURL  = "https://raw.githubusercontent.com/Austrum-lab/steam-appdb/master/data/all.json"
	storeSearch = "https://store.steampowered.com/api/storesearch/"
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

// searchClient 用于搜索回退的短超时客户端，避免国内超时卡很久。
var searchClient = &http.Client{Timeout: 5 * time.Second}

// steamDir 是用户配置的 Steam 运行目录，下载的 .lua 文件写入这里。
var steamDir string

// downloadManifests 表示是否把 depot manifest 一并下到 depotcache（由配置决定）。
var downloadManifests = true

// version 由构建时注入：-ldflags "-X main.version=v1.0.0"。
var version = "dev"

// stdin 全局共享一个带缓冲的读取器。多次新建 bufio.Scanner(os.Stdin) 会各自
// 预读缓冲，导致管道输入时后续提示读不到数据。
var stdin = bufio.NewReader(os.Stdin)

// readLine 读取一行（去掉行尾换行）；EOF 且无内容时返回 ok=false。
func readLine() (string, bool) {
	line, err := stdin.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", false
	}
	return line, true
}

// ---------------------------------------------------------------- 路径与日志

// 配置目录固定为 <用户家目录>/.config/lua4ost/，三个文件路径在 setupPaths 里算出。
var (
	configDir   string
	configFile  string
	appListFile string
	logFile     string
)

// setupPaths 计算配置目录（~/.config/lua4ost）并创建；家目录取不到时退化为当前目录。
func setupPaths() {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	dir := filepath.Join(home, ".config", "lua4ost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = "."
	}
	configDir = dir
	configFile = filepath.Join(dir, "config.json")
	appListFile = filepath.Join(dir, "applist.json")
	logFile = filepath.Join(dir, "lua4ost.log")
}

// migrateLegacy 把旧版本放在工作目录下的 config.json / applist.json 搬到配置目录。
func migrateLegacy() {
	if _, err := os.Stat(configFile); err != nil {
		if data, err := os.ReadFile("config.json"); err == nil {
			if err := os.WriteFile(configFile, data, 0o644); err == nil {
				fmt.Fprintf(os.Stderr, "已把 config.json 迁移到 %s\n", configFile)
			}
		}
	}
	if _, err := os.Stat(appListFile); err != nil {
		if _, err := os.Stat("applist.json"); err == nil {
			_ = os.Rename("applist.json", appListFile) // 同盘瞬间完成；跨盘失败就重新拉
		}
	}
}

// logger 写 <配置目录>/lua4ost.log；打不开时退化为不记录，绝不影响主流程。
var logger *log.Logger

func setupLogging() {
	const maxLogSize = 2 << 20 // 2MB 后轮转一份 .1，避免无限增长
	if fi, err := os.Stat(logFile); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(logFile, logFile+".1")
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	logger = log.New(f, "", log.LstdFlags|log.Lmicroseconds)
}

// logf 写一行日志；未初始化时静默丢弃。
func logf(format string, args ...any) {
	if logger != nil {
		logger.Printf(format, args...)
	}
}

// ---------------------------------------------------------------- 入口

func main() {
	setupPaths()

	// 版本查询不需要任何配置，放在最前面。
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			printVersion()
			return
		}
	}

	setupLogging()
	migrateLegacy()
	logf("===== 启动 lua4ost %s (%s/%s) 配置目录=%s args=%v =====",
		version, runtime.GOOS, runtime.GOARCH, configDir, os.Args[1:])
	defer logf("===== 退出 =====")

	if len(os.Args) > 2 {
		fmt.Fprintf(os.Stderr, "用法: %s [appid|游戏名|update|--version]\n", os.Args[0])
		os.Exit(1)
	}

	isUpdate := len(os.Args) == 2 && os.Args[1] == "update"

	// 确保已配置 Steam 运行目录（首次启动交互式指定）。
	dir, err := ensureSteamDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	steamDir = dir

	// update 命令：刷新游戏列表 + 升级 OpenSteamTool。
	if isUpdate {
		if err := updateAppList(); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		fmt.Println("已更新", appListFile)
		if err := updateToolCLI(steamDir); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		return
	}

	// 确保 OpenSteamTool 已安装（未装则装；有更新仅标记，不自动重装）。
	autoCheck, wantManifest := true, true
	if cfg, err := loadConfig(); err == nil {
		autoCheck = cfg.autoCheckUpdate()
		wantManifest = cfg.downloadManifest()
	}
	downloadManifests = wantManifest
	updateAvailable, err := ensureTool(steamDir, autoCheck)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	if updateAvailable {
		fmt.Fprintln(os.Stderr, "提示: OpenSteamTool 有新版本，可在 TUI 中更新（Tab 跳到该行后回车）")
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
	if err := runApp(initial, apps, updateAvailable, autoCheck); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- 配置

// printVersion 输出版本号与 OpenSteamTool 状态；不写任何配置、不交互。
func printVersion() {
	fmt.Println("lua4ost", version)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Steam 目录: 配置读取失败:", err)
		return
	}
	if cfg.SteamDir == "" {
		fmt.Println("Steam 目录: 未配置（首次运行时会提示设置）")
		fmt.Println("配置目录:", configDir)
		fmt.Println("日志文件:", logFile)
		return
	}
	dirLine := cfg.SteamDir
	if !isDir(cfg.SteamDir) {
		dirLine += "（目录不存在）"
	}
	fmt.Println("Steam 目录:", dirLine)
	fmt.Println("配置目录:", configDir)
	fmt.Println("日志文件:", logFile)

	installed := installedToolVersion(cfg.SteamDir)
	_, statErr := os.Stat(filepath.Join(cfg.SteamDir, ostFlagName))
	switch {
	case statErr != nil:
		fmt.Println("OpenSteamTool: 未安装")
	case installed == "":
		fmt.Println("OpenSteamTool: 已安装（flag 未记录版本）")
	default:
		fmt.Println("OpenSteamTool: v" + installed)
	}

	if !cfg.autoCheckUpdate() {
		fmt.Println("  最新版本: 未查询（已关闭自动检查更新）")
		return
	}
	latest, err := latestTag(5 * time.Second)
	if err != nil {
		fmt.Println("  最新版本: 查询失败")
		return
	}
	switch {
	case installed == "":
		fmt.Printf("  最新版本: v%s\n", latest)
	case installed == latest:
		fmt.Printf("  最新版本: v%s（已是最新）\n", latest)
	default:
		fmt.Printf("  最新版本: v%s（有更新：v%s → v%s）\n", latest, installed, latest)
	}
}

type config struct {
	SteamDir         string `json:"steam_dir"`
	AutoCheckUpdate  *bool  `json:"auto_check_update,omitempty"`
	DownloadManifest *bool  `json:"download_manifest,omitempty"`
}

// autoCheckUpdate 表示是否自动检查 OpenSteamTool 更新；默认开启
// （旧配置文件没有该字段时视为开启）。
func (c config) autoCheckUpdate() bool {
	if c.AutoCheckUpdate == nil {
		return true
	}
	return *c.AutoCheckUpdate
}

// downloadManifest 表示是否把 depot manifest 一并下载到 depotcache；默认开启。
func (c config) downloadManifest() bool {
	if c.DownloadManifest == nil {
		return true
	}
	return *c.DownloadManifest
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
		line, ok := readLine()
		if !ok {
			return "", fmt.Errorf("读取输入失败")
		}
		dir := expandPath(strings.TrimSpace(line))
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
		// 首次设置时一并选择是否自动检查更新
		auto := promptYesNo("是否自动检查 OpenSteamTool 更新? [Y/n]: ", true)
		cfg.AutoCheckUpdate = &auto
		dm := promptYesNo("是否下载 depot manifest 到 depotcache? [Y/n]: ", true)
		cfg.DownloadManifest = &dm
		logf("首次配置: steam_dir=%s auto_check_update=%v download_manifest=%v", abs, auto, dm)
		if err := saveConfig(cfg); err != nil {
			return "", err
		}
		return abs, nil
	}
}

// promptYesNo 读取一行 y/n 回答；空输入或无法识别时用默认值。
func promptYesNo(prompt string, def bool) bool {
	fmt.Print(prompt)
	line, ok := readLine()
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "是":
		return true
	case "n", "no", "否":
		return false
	}
	return def
}

// expandPath 展开 ~ 和环境变量。
// Windows 自身不认 ~（那是 Unix shell 的约定），这里由我们主动展开：
// os.UserHomeDir() 在 Windows 上取 %USERPROFILE%，因此 ~ / ~\ 同样可用。
// 另外额外支持 Windows 风格的 %VAR%（os.ExpandEnv 只认 $VAR/${VAR}）。
func expandPath(p string) string {
	p = strings.TrimSpace(p)
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
	p = os.ExpandEnv(p)
	if runtime.GOOS == "windows" {
		p = expandPercentEnv(p)
	}
	return p
}

var rePercentEnv = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_]*)%`)

func expandPercentEnv(s string) string {
	return rePercentEnv.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := os.LookupEnv(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	})
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
// installedToolVersion 读取已安装的 OpenSteamTool 版本；未安装返回 ""。
func installedToolVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ostFlagName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ensureTool 确保 OpenSteamTool 已安装；autoCheck 为 true 时顺带检查最新版本，
// 返回是否有可用更新（任何时候都不自动重装）。
func ensureTool(dir string, autoCheck bool) (bool, error) {
	installed := installedToolVersion(dir)
	logf("OpenSteamTool: 目录=%s 已装版本=%q autoCheck=%v", dir, installed, autoCheck)
	if installed == "" {
		if err := installToolCLI(dir); err != nil {
			logf("OpenSteamTool 安装失败: %v", err)
			return false, err
		}
		return false, nil
	}
	if !autoCheck {
		logf("OpenSteamTool: 已关闭自动检查，跳过版本查询")
		return false, nil
	}
	latest, err := latestTag(5 * time.Second)
	if err != nil {
		logf("OpenSteamTool: 查询最新版本失败（按无更新处理）: %v", err)
		return false, nil
	}
	has := installed != latest
	logf("OpenSteamTool: 已装=%s 最新=%s 有更新=%v", installed, latest, has)
	return has, nil
}

// installToolCLI 交互式安装/升级 OpenSteamTool（检测 Steam 运行并等待退出）。
func installToolCLI(dir string) error {
	if err := ensureSteamClosed(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "正在获取 OpenSteamTool 最新发布...")
	version, err := doInstallTool(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "已安装 OpenSteamTool v%s\n", version)
	return nil
}

// updateToolCLI 供 update 命令使用：升级到最新版（已是最新则跳过）。
func updateToolCLI(dir string) error {
	installed := installedToolVersion(dir)
	latest, err := latestTag(15 * time.Second)
	if err != nil {
		return err
	}
	if installed == latest {
		fmt.Printf("OpenSteamTool 已是最新 (v%s)\n", latest)
		return nil
	}
	if installed == "" {
		fmt.Printf("未安装 OpenSteamTool，开始安装 v%s\n", latest)
	} else {
		fmt.Printf("OpenSteamTool 有更新: v%s -> v%s，开始升级\n", installed, latest)
	}
	return installToolCLI(dir)
}

// doInstallTool 下载最新版并解压，flag 写入版本号。返回版本号（不打印、不检测 Steam）。
func doInstallTool(dir string) (string, error) {
	tag, err := latestTag(15 * time.Second)
	if err != nil {
		return "", err
	}
	assets, err := releaseAssets(tag)
	if err != nil {
		return "", err
	}
	asset, err := pickZipAsset(assets)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp("", "opensteamtool-*.zip")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := downloadFile(asset.BrowserDownloadURL, tmpPath); err != nil {
		return "", err
	}
	// 解压前预检目标文件是否可写：被占用（Steam/游戏仍在运行）就直接报错，
	// 避免解压到一半失败、留下 DLL 半新半旧的烂摊子。
	if err := checkWritable(tmpPath, dir); err != nil {
		return "", err
	}
	if err := extractZip(tmpPath, dir); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ostFlagName), []byte(tag), 0o644); err != nil {
		return "", err
	}
	return tag, nil
}

// checkWritable 逐个尝试以写方式打开 zip 中「已存在」的目标文件。
// Windows 上被进程加载的 DLL 不允许写入，会在这里就失败；Unix 无此锁，恒为可写。
func checkWritable(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) || f.FileInfo().IsDir() {
			continue
		}
		target := filepath.Join(dest, name)
		if _, err := os.Stat(target); err != nil {
			continue // 目标不存在，无需预检
		}
		fh, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			logf("可写性预检失败: %s 被占用: %v", target, err)
			return fmt.Errorf("无法覆盖 %s（文件被占用，请先退出 Steam 及相关游戏）: %w", name, err)
		}
		fh.Close()
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
		if _, ok := readLine(); !ok {
			return fmt.Errorf("读取输入失败")
		}
		if !isSteamRunning() {
			return nil
		}
		fmt.Println("Steam 仍在运行。")
	}
}

func latestTag(timeout time.Duration) (string, error) {
	client := &http.Client{
		Timeout: timeout,
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
	logf("OpenSteamTool latest tag = %s", tag)
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
	start := time.Now()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		logf("GET %s 失败: %v", url, err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		logf("GET %s -> HTTP %d", url, resp.StatusCode)
		return fmt.Errorf("下载失败 (HTTP %d)", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		logf("GET %s 写入 %s 失败: %v", url, dst, err)
		return err
	}
	logf("GET %s -> HTTP 200, %d 字节 -> %s (%v)", url, n, dst, time.Since(start).Round(time.Millisecond))
	return nil
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
		logf("本地 app 列表不存在，开始拉取")
		if err := fetchAppList(); err != nil {
			logf("app 列表拉取失败: %v", err)
			return nil, err
		}
	}
	apps, err := readAppList(appListFile)
	if err != nil {
		logf("app 列表解析失败 (%s): %v", appListFile, err)
		return nil, err
	}
	logf("已载入 app 列表 %s: %d 条", appListFile, len(apps))
	return apps, nil
}

// updateAppList 强制重新拉取（手动 update 命令用）。
func updateAppList() error {
	fmt.Fprintf(os.Stderr, "正在从 GitHub 更新 %s ...\n", appListFile)
	return fetchAppList()
}

func fetchAppList() error {
	start := time.Now()
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
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	logf("GET %s -> HTTP 200, %d 字节 -> %s (%v)", appListURL, n, appListFile, time.Since(start).Round(time.Millisecond))
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
	start := time.Now()

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
	// 本地搜索每次按键都会跑，只在无结果（会触发商店回退）时记录，避免刷屏
	if len(out) == 0 {
		logf("搜索[本地] %q -> 0 条 (库 %d 条, 耗时 %v)", q, len(apps), time.Since(start).Round(time.Millisecond))
	}
	return out
}

// searchRemote 是本地搜索 0 结果时的回退：调 Steam 商店搜索（短超时）。
func searchRemote(name string) ([]appEntry, error) {
	start := time.Now()
	langs := [][2]string{{"english", "US"}, {"schinese", "CN"}}
	if isCJK(name) {
		langs[0], langs[1] = langs[1], langs[0]
	}
	var lastErr error
	for _, l := range langs {
		items, err := searchStore(name, l[0], l[1])
		if err != nil {
			logf("搜索[商店] %q l=%s cc=%s 失败: %v", name, l[0], l[1], err)
			lastErr = err
			continue
		}
		logf("搜索[商店] %q l=%s cc=%s -> %d 条", name, l[0], l[1], len(items))
		if len(items) > 0 {
			logf("搜索[商店] %q 命中 (总耗时 %v)", name, time.Since(start).Round(time.Millisecond))
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
	logf("GET %s?term=%s&l=%s&cc=%s -> HTTP %d", storeSearch, url.QueryEscape(name), lang, cc, resp.StatusCode)

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
	Manifests  int // 已写入 depotcache 的 manifest 个数
	Skipped    int // 仓库里没有对应 manifest（多为共享 depot）而跳过的个数
}

func (r downloadResult) String() string {
	if r.Source == "GitHub" {
		s := fmt.Sprintf("%s (%s, %d bytes)", r.File, r.Source, r.Size)
		if r.Manifests > 0 || r.Skipped > 0 {
			s += fmt.Sprintf(" + manifest %d", r.Manifests)
			if r.Skipped > 0 {
				s += fmt.Sprintf("(跳过 %d 个无 manifest 的共享 depot)", r.Skipped)
			}
		}
		return s
	}
	s := fmt.Sprintf("%s (%s, %d bytes | PoW %d 次 %dms | difficulty=%d)",
		r.File, r.Source, r.Size, r.Nonce, r.PowMs, r.Difficulty)
	if r.Manifests > 0 {
		s += fmt.Sprintf(" + manifest %d", r.Manifests)
	}
	return s
}

// download 优先从 GitHub ManifestHub 下载，拿不到再回退 Walftech。
func download(appid, name string) (downloadResult, error) {
	start := time.Now()
	logf("下载开始 appid=%s name=%q (manifest=%v)", appid, name, downloadManifests)

	if out, size, ok := downloadGitHub(appid, name); ok {
		res := downloadResult{Source: "GitHub", File: out, Size: size}
		// lua 里用 setManifestid 固定了 depot 版本，顺带把这些 manifest 拉进 depotcache。
		if downloadManifests {
			if dir, err := luaOutputDir(); err == nil {
				if lua, err := os.ReadFile(filepath.Join(dir, out)); err == nil {
					res.Manifests, res.Skipped = syncManifests(appid, lua)
				}
			}
		}
		logf("下载完成 appid=%s 来源=GitHub 文件=%s 大小=%d manifest=%d 跳过=%d 总耗时=%v",
			appid, out, size, res.Manifests, res.Skipped, time.Since(start).Round(time.Millisecond))
		return res, nil
	}

	logf("ManifestHub 无该分支，回退 Walftech (appid=%s)", appid)
	challenge, difficulty, err := getChallenge()
	if err != nil {
		logf("下载失败 appid=%s 阶段=challenge: %v", appid, err)
		return downloadResult{}, err
	}
	t0 := time.Now()
	nonce := solvePow(challenge, difficulty)
	powMs := time.Since(t0).Milliseconds()
	logf("PoW appid=%s difficulty=%d nonce=%d 耗时=%dms", appid, difficulty, nonce, powMs)

	token, err := redeem(appid, challenge, nonce)
	if err != nil {
		logf("下载失败 appid=%s 阶段=redeem: %v", appid, err)
		return downloadResult{}, err
	}
	out, size, manifests, err := downloadLuaFull(appid, token, name)
	if err != nil {
		logf("下载失败 appid=%s 阶段=walftech下载: %v", appid, err)
		return downloadResult{}, err
	}
	logf("下载完成 appid=%s 来源=Walftech 文件=%s lua大小=%d manifest=%d 总耗时=%v",
		appid, out, size, manifests, time.Since(start).Round(time.Millisecond))
	return downloadResult{
		Source:     "Walftech",
		File:       out,
		Size:       size,
		Manifests:  manifests,
		Nonce:      nonce,
		PowMs:      powMs,
		Difficulty: difficulty,
	}, nil
}

func downloadGitHub(appid, name string) (string, int64, bool) {
	u := fmt.Sprintf(githubRaw, appid, appid)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		logf("GET %s 构造请求失败: %v", u, err)
		return "", 0, false
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		logf("GET %s 失败: %v", u, err)
		return "", 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		logf("GET %s -> HTTP %d（该 appid 无 ManifestHub 分支）", u, resp.StatusCode)
		return "", 0, false
	}

	basename := luaFilename(appid, name)
	dir, err := luaOutputDir()
	if err != nil {
		logf("创建 lua 目录失败: %v", err)
		return "", 0, false
	}
	dest := filepath.Join(dir, basename)
	size, err := saveFile(dest, resp.Body)
	if err != nil {
		logf("写入 %s 失败: %v", dest, err)
		return "", 0, false
	}
	logf("GET %s -> HTTP 200, 已保存 %s (%d 字节)", u, dest, size)
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

// ---------------------------------------------------------------- Manifest

// reSetManifest 匹配 lua 里的 setManifestid(<depot>,"<gid>")。
var reSetManifest = regexp.MustCompile(`setManifestid\(\s*(\d+)\s*,\s*"(\d+)"\s*\)`)

type manifestRef struct {
	Depot string
	GID   string
}

func parseManifests(lua []byte) []manifestRef {
	var refs []manifestRef
	seen := make(map[string]bool)
	for _, m := range reSetManifest.FindAllStringSubmatch(string(lua), -1) {
		key := m[1] + "_" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, manifestRef{Depot: m[1], GID: m[2]})
	}
	return refs
}

// syncManifests 把 lua 里声明的 depot manifest 下载到 <Steam>/depotcache/。
// 仓库里没有对应文件的（多为 228980 系列共享 depot）计入 skipped。
func syncManifests(appid string, lua []byte) (ok, skipped int) {
	refs := parseManifests(lua)
	if len(refs) == 0 {
		logf("manifest 同步 appid=%s: lua 里没有 setManifestid", appid)
		return 0, 0
	}
	dir := filepath.Join(steamDir, "depotcache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("manifest 同步 appid=%s: 创建 %s 失败: %v", appid, dir, err)
		return 0, len(refs)
	}
	logf("manifest 同步 appid=%s: lua 声明了 %d 个 depot", appid, len(refs))
	for _, r := range refs {
		name := r.Depot + "_" + r.GID + ".manifest"
		dest := filepath.Join(dir, name)
		if _, err := os.Stat(dest); err == nil {
			ok++ // 已存在，跳过下载
			logf("  manifest %s 已存在，跳过", name)
			continue
		}
		u := fmt.Sprintf(manifestRaw, appid, name)
		if err := downloadFile(u, dest); err != nil {
			os.Remove(dest)
			skipped++
			logf("  manifest %s 下载失败（共享 depot 通常是 404）: %v", name, err)
			continue
		}
		fi, _ := os.Stat(dest)
		var sz int64
		if fi != nil {
			sz = fi.Size()
		}
		ok++
		logf("  manifest %s 已保存 (%d 字节)", name, sz)
	}
	logf("manifest 同步 appid=%s: 成功 %d, 跳过 %d", appid, ok, skipped)
	return ok, skipped
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

// deleteLuaFile 删除 lua，并清理因此变成孤儿的 depot manifest：
// 仍被其它 lua 引用的 manifest 会保留（例如 228990_1829726630299308803 就常被多个游戏共用）。
// 返回一并删除的 manifest 个数。
func deleteLuaFile(name string) (int, error) {
	if name == "" || filepath.Base(name) != name {
		return 0, fmt.Errorf("非法文件名: %s", name)
	}
	dir, err := luaOutputDir()
	if err != nil {
		return 0, err
	}
	luaPath := filepath.Join(dir, name)

	// 删之前先记下它引用了哪些 manifest
	var mine []manifestRef
	if data, err := os.ReadFile(luaPath); err == nil {
		mine = parseManifests(data)
	}

	if err := os.Remove(luaPath); err != nil {
		return 0, err
	}

	// 剩余 lua 仍在引用的 manifest 集合
	still := make(map[string]bool)
	if files, err := listLuaFiles(); err == nil {
		for _, f := range files {
			data, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				continue
			}
			for _, r := range parseManifests(data) {
				still[r.Depot+"_"+r.GID] = true
			}
		}
	}

	depotDir := filepath.Join(steamDir, "depotcache")
	removed := 0
	for _, r := range mine {
		key := r.Depot + "_" + r.GID
		if still[key] {
			logf("删除 lua %s: manifest %s 仍被其它 lua 引用，保留", name, key)
			continue
		}
		if err := os.Remove(filepath.Join(depotDir, key+".manifest")); err == nil {
			removed++
			logf("删除 lua %s: 一并删除 manifest %s.manifest", name, key)
		}
	}
	logf("删除 lua %s 完成（清理 manifest %d 个）", name, removed)
	return removed, nil
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

// downloadLuaFull 走 Walftech 保底分支，用 format=full 一次拿到 lua + manifests 的 zip：
// lua 写入 <Steam>/config/lua/，manifest 写入 <Steam>/depotcache/。
// 返回 lua 文件名、lua 大小、写入的 manifest 个数。
func downloadLuaFull(appid, token, name string) (string, int64, int, error) {
	start := time.Now()
	q := url.Values{}
	q.Set("id", appid)
	q.Set("token", token)
	format := "lua"
	if downloadManifests {
		format = "full" // zip：lua + 全部 manifest
	}
	q.Set("format", format)
	logf("GET %s?%s", baseURL+"/depotbox_lua.php", "id="+appid+"&format="+format+"&token=***")

	req, err := walftechRequest("GET", "/depotbox_lua.php", nil, q)
	if err != nil {
		return "", 0, 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		logf("GET depotbox_lua.php(appid=%s) 失败: %v", appid, err)
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		logf("GET depotbox_lua.php(appid=%s) -> HTTP %d: %s", appid, resp.StatusCode, string(data))
		return "", 0, 0, fmt.Errorf("下载失败 (HTTP %d): %s", resp.StatusCode, string(data))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, err
	}
	logf("GET depotbox_lua.php(appid=%s) -> HTTP 200, format=%s, %d 字节", appid, format, len(body))

	luaDir, err := luaOutputDir()
	if err != nil {
		return "", 0, 0, err
	}
	luaName := luaFilename(appid, name)
	luaPath := filepath.Join(luaDir, luaName)

	// 个别 appid 可能只返回纯 lua 而不是 zip，直接落盘即可。
	if len(body) < 2 || string(body[:2]) != "PK" {
		if err := os.WriteFile(luaPath, body, 0o644); err != nil {
			return "", 0, 0, err
		}
		logf("walftech 返回纯 lua，已保存 %s (%d 字节, %v)", luaPath, len(body), time.Since(start).Round(time.Millisecond))
		return luaName, int64(len(body)), 0, nil
	}

	depotDir := filepath.Join(steamDir, "depotcache")
	if err := os.MkdirAll(depotDir, 0o755); err != nil {
		return "", 0, 0, err
	}

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", 0, 0, err
	}

	var luaSize int64
	manifests := 0
	for _, zf := range zr.File {
		base := filepath.Base(zf.Name)
		switch {
		case strings.HasSuffix(strings.ToLower(base), ".lua"):
			n, err := extractZipEntry(zf, luaPath)
			if err != nil {
				return "", 0, 0, err
			}
			luaSize = n
		case strings.HasSuffix(strings.ToLower(base), ".manifest"):
			dest := filepath.Join(depotDir, base)
			if _, err := os.Stat(dest); err == nil {
				manifests++
				continue // 已存在，不重复写
			}
			if _, err := extractZipEntry(zf, dest); err != nil {
				continue
			}
			manifests++
		}
	}
	if luaSize == 0 {
		return "", 0, 0, fmt.Errorf("zip 里没有 .lua 文件")
	}
	logf("walftech zip 解包完成 appid=%s: lua=%s (%d 字节), manifest=%d (%v)",
		appid, luaName, luaSize, manifests, time.Since(start).Round(time.Millisecond))
	return luaName, luaSize, manifests, nil
}

// extractZipEntry 把 zip 中的单个条目写到 dest。
func extractZipEntry(zf *zip.File, dest string) (int64, error) {
	rc, err := zf.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, rc)
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
