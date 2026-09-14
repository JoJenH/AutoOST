# Lua4OST

Steam 游戏 Lua 下载器（Go 实现，编译成单文件二进制）。

自动安装 [OpenSteamTool](https://github.com/OpenSteam001/OpenSteamTool)，从 GitHub `steamtoolsapp/ManifestHub` 下载游戏 Lua（分支不存在时回退 Walftech），存放到 Steam 的 `config/lua` 目录，并内置 TUI 用于搜索、下载、查看与删除。

## 功能特性

- **首次启动配置**：指定 Steam 运行目录，并选择是否自动检查更新，保存到 `config.json`。
- **自动安装 OpenSteamTool**：无 `.AutoOST.flag` 时自动下载最新发布并解压到 Steam 根目录（Windows 下会先检测 Steam 是否在运行）。
- **版本感知更新**：`.AutoOST.flag` 记录已装版本；自动检查（可关闭）发现有新版只提示，由你决定何时更新。
- **离线搜索**：本地缓存全量 Steam app 列表（`applist.json`），搜索零网络、不会超时；本地无结果时自动回退 Steam 商店搜索（5 秒短超时）。
- **双源下载**：优先 GitHub ManifestHub，失败回退 Walftech（复刻 `generator.html` 的 PoW 门禁）。
- **附带 manifest**：主路径从 ManifestHub 分支拉取 lua 用 `setManifestid` 固定的 depot manifest；保底路径则用 Walftech 的 `format=full`（lua + manifest 的 zip）。两者都写入 `<Steam>/depotcache/`，可用 `download_manifest` 关闭。
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
./lua4ost update              # 刷新游戏列表，并把 OpenSteamTool 升级到最新版
./lua4ost                     # 进入 TUI，搜索并下载
./lua4ost "Counter-Strike 2"  # 进入 TUI，预填游戏名并自动搜索
```

## 首次启动流程

1. 提示输入 **Steam 运行目录**（支持 `~` 和环境变量），校验必须为已存在的文件夹。
2. 依次询问两个开关（都直接回车即为开启，与目录一起写入 `config.json`）：
   - **是否自动检查 OpenSteamTool 更新**（`[Y/n]`）；
   - **是否下载 depot manifest 到 depotcache**（`[Y/n]`）。
3. 检查 `<Steam>/.AutoOST.flag`：不存在则自动安装 OpenSteamTool：
   - Windows 下检测 Steam 进程，运行中会提示先退出；
   - 从 `OpenSteam001/OpenSteamTool` 最新发布下载 zip（不依赖 GitHub API）；
   - 解压 `dwmapi.dll` / `xinput1_4.dll` / `OpenSteamTool.dll` 等到 Steam 根目录；
   - 写入 `.AutoOST.flag`（内容为版本号）。
4. 之后每次启动直接跳过安装，正常使用。

> 想重装 OpenSteamTool：退出 Steam，删除 `<Steam>/.AutoOST.flag` 后重新运行。

## OpenSteamTool 更新

- `.AutoOST.flag` 里记录**已安装版本号**（如 `1.4.8`），用于与最新版本对比。
- **自动检查**由首次设置时选择（也可直接改 `config.json` 的 `auto_check_update`）：
  - **开启**：启动时对比最新版本，有新版只在终端提示一行，并在 TUI 列表下方显示 `⚠ OpenSteamTool 有新版本`；
  - **关闭**：启动不联网检查，TUI 列表下方保留「检查 OpenSteamTool 更新」手动入口。
- 两种模式都可**按回车**执行检查（先判断是否最新，需要才安装）。
- CLI 也可用 `./lua4ost update` 直接升级。
- 更新保护：Windows 下先检测 Steam 是否运行；解压前还会逐个预检目标 DLL 是否可写，被占用时直接报错并指出文件名，避免解压到一半留下半写状态。

## TUI 按键

### 搜索模式

| 按键 | 作用 |
|------|------|
| 输入 | 输入游戏名（中英文）或 appid，本地离线搜索 |
| `↑` / `↓`（`j` / `k`） | 选择结果 |
| `回车` | 下载选中游戏（输入框为数字时直接下载该 id） |
| `Tab` | 依次跳到动作行：搜索 Steam 商店 → OpenSteamTool（若有） |
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

### Manifest 下载

两条 lua 来源都会把 manifest 一并写进 `<Steam>/depotcache/`，产出目录完全一致。
该行为由 `config.json` 的 `download_manifest` 控制（首次设置时可选，默认开启）；**关闭后两条路径都不会下载 manifest**，保底路径也会退回体积小得多的 `format=lua`。

**1) GitHub ManifestHub（主路径）** —— 解析刚下载的 lua 里的 `setManifestid(<depot>,"<gid>")`，逐个取：

```
https://github.com/steamtoolsapp/ManifestHub/raw/refs/heads/<appid>/<depot>_<gid>.manifest
```

- `<depot>` / `<gid>` 来自 lua（也可见于分支的 `<appid>.json`）。
- **只有游戏自己的、带解密 key 的 depot 才有 manifest**；共享 depot（`depotfromapp 228980` 的 228989/228990 等）在仓库里是 404，会自动跳过并在输出里计数。
- ManifestHub 数据本身可能缺项（例如 `2087460` 分支的 lua 漏了 DLC depot `2868020`），工具只跟随 lua 的声明。

**2) Walftech 保底分支** —— 该 appid 在 ManifestHub 没有分支时，改请求 `depotbox_lua.php?...&format=full`，它直接返回一个 **lua + 全部 manifest 的 zip**：

- `*.lua` → `<Steam>/config/lua/<名字>-<appid>.lua`
- `*.manifest` → `<Steam>/depotcache/`（文件名本身已是 `<depot>_<gid>.manifest`）
- 已存在的 manifest 不重复写；zip 里的说明 txt 等其它文件会忽略。

## 生成的文件

| 文件 | 说明 |
|------|------|
| `config.json` | 保存 `steam_dir`、`auto_check_update`、`download_manifest`（已 gitignore） |
| `applist.json` | 本地游戏列表缓存（已 gitignore） |
| `<Steam>/.AutoOST.flag` | OpenSteamTool 已安装标记 |
| `<Steam>/config/lua/*.lua` | 下载的 Lua 文件 |
| `<Steam>/depotcache/<depot>_<gid>.manifest` | 下载的 depot manifest |

## 依赖

- Go 1.24+
- [bubbletea](https://github.com/charmbracelet/bubbletea) + [lipgloss](https://github.com/charmbracelet/lipgloss) + [bubbles](https://github.com/charmbracelet/bubbles)（TUI）
