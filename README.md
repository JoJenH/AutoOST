# Lua4OST

Steam 游戏 Lua 下载器（Go 实现，编译成单文件二进制）。

自动安装 [OpenSteamTool](https://github.com/OpenSteam001/OpenSteamTool)，从 GitHub `steamtoolsapp/ManifestHub` 下载游戏 Lua（分支不存在时回退 Walftech），存放到 Steam 的 `config/lua` 目录，并内置 TUI 用于搜索、下载、查看与删除。

## 功能特性

- **首次启动配置**：要求指定 Steam 运行目录，保存到 `config.json`。
- **自动安装 OpenSteamTool**：无 `.AutoOST.flag` 时自动下载最新发布并解压到 Steam 根目录（Windows 下会先检测 Steam 是否在运行）。
- **离线搜索**：本地缓存全量 Steam app 列表（`applist.json`），搜索零网络、不会超时；本地无结果时自动回退 Steam 商店搜索（5 秒短超时）。
- **双源下载**：优先 GitHub ManifestHub，失败回退 Walftech（复刻 `generator.html` 的 PoW 门禁）。
- **TUI 管理**：搜索下载、查看已安装 Lua、查看文件内容、删除。

## 构建

需要 Go 1.24+。

```bash
go build -o lua4ost .
```

交叉编译到 Windows：

```bash
GOOS=windows GOARCH=amd64 go build -o lua4ost.exe .
```

## 用法

```bash
./lua4ost 730                 # 直接下载 730.lua 后退出
./lua4ost update              # 强制刷新本地游戏列表 applist.json
./lua4ost                     # 进入 TUI，搜索并下载
./lua4ost "Counter-Strike 2"  # 进入 TUI，预填游戏名并自动搜索
```

## 首次启动流程

1. 提示输入 **Steam 运行目录**（支持 `~` 和环境变量），校验为文件夹后写入 `config.json`。
2. 检查 `<Steam>/.AutoOST.flag`：不存在则自动安装 OpenSteamTool：
   - Windows 下检测 Steam 进程，运行中会提示先退出；
   - 从 `OpenSteam001/OpenSteamTool` 最新发布下载 zip（不依赖 GitHub API）；
   - 解压 `dwmapi.dll` / `xinput1_4.dll` / `OpenSteamTool.dll` 等到 Steam 根目录；
   - 写入 `.AutoOST.flag`。
3. 之后每次启动直接跳过安装，正常使用。

> 想重装/更新 OpenSteamTool：退出 Steam，删除 `<Steam>/.AutoOST.flag` 后重新运行。

## TUI 按键

### 搜索模式

| 按键 | 作用 |
|------|------|
| 输入 | 输入游戏名（中英文）或 appid，本地离线搜索 |
| `↑` / `↓`（`j` / `k`） | 选择结果 |
| `回车` | 下载选中游戏（输入框为数字时直接下载该 id） |
| `Tab` | 跳转到「没有想要的结果？→ 搜索 Steam 商店」 |
| `Ctrl+L` | 进入已安装 Lua 管理 |
| `Esc` | 清空输入；输入为空时退出 |
| `Ctrl-C` | 退出 |

### 已安装 Lua 管理（`Ctrl+L`）

| 按键 | 作用 |
|------|------|
| `↑` / `↓` | 选择文件 |
| `回车` | 查看文件内容 |
| `d` / `Delete` | 删除（二次确认：再按一次才真正删除） |
| `Esc` | 返回搜索模式 |

### 内容查看

| 按键 | 作用 |
|------|------|
| `↑` / `↓`（`j` / `k`） | 逐行滚动 |
| `PgUp` / `PgDn` | 翻页 |
| `Esc` / `q` / `回车` | 返回列表 |

## 搜索机制

- 首次运行（或 `update`）从 `Austrum-lab/steam-appdb` 拉取全量 app 列表（`all.json`，约 25MB），缓存到本地 `applist.json`，只保留 `game` / `dlc` / `music` 三类。
- 搜索为**本地模糊匹配**（忽略大小写，精确 > 前缀 > 子串，游戏优先），零网络、不受 Steam 接口超时影响。
- 本地 0 结果时自动回退 Steam 商店搜索（语言自适应：中文走简中、否则英文，5 秒短超时）；也可在列表底部手动触发。

## 下载流程

1. `GET github.com/steamtoolsapp/ManifestHub/raw/refs/heads/<id>/<id>.lua`，成功直接保存。
2. 分支不存在则回退 Walftech：
   - `POST /gate.php {"action":"challenge"}` 拿 `challenge` + `difficulty`
   - 暴力找 `nonce`，使 `sha256(challenge:nonce)` 前缀满足 `difficulty` 个 `0`（PoW）
   - `POST /gate.php {"action":"redeem", ...}` 换 `token`
   - `GET /depotbox_lua.php?id=<id>&token=<token>&format=lua` 下载

请求头必须带 `Referer: https://walftech.com/generator.html`，否则 403。

下载文件保存为 `<Steam>/config/lua/<名字>-<appid>.lua`（名称含非法字符会自动替换；CLI 只传 id 时保存为 `<appid>.lua`）。OpenSteamTool 只按 `.lua` 扩展名加载，文件名不影响功能（appid 由内容里的 `addappid()` 决定）。

## 生成的文件

| 文件 | 说明 |
|------|------|
| `config.json` | 保存 `steam_dir`（已 gitignore） |
| `applist.json` | 本地游戏列表缓存（已 gitignore） |
| `<Steam>/.AutoOST.flag` | OpenSteamTool 已安装标记 |
| `<Steam>/config/lua/*.lua` | 下载的 Lua 文件 |

## 依赖

- Go 1.24+
- [bubbletea](https://github.com/charmbracelet/bubbletea) + [lipgloss](https://github.com/charmbracelet/lipgloss) + [bubbles](https://github.com/charmbracelet/bubbles)（TUI）
