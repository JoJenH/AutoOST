# Lua4OST

Steam 游戏 Lua 下载器（Go 实现，可编译成单文件二进制）。

下载时优先从 GitHub `steamtoolsapp/ManifestHub` 取，分支不存在时回退到 Walftech（复刻 `generator.html` 的 PoW 门禁 + 下载流程）。

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
./lua4ost 730                 # 单条命令：直接下载 730.lua 后退出
./lua4ost                     # 进入 TUI，搜索并下载
./lua4ost "Counter-Strike 2"  # 进入 TUI，预填游戏名并自动搜索
```

### 交互模式（TUI）

- 顶部输入框输入游戏名（支持中英文，实时搜索），或直接输入 appid。
- `↑`/`↓`（或 `j`/`k`）选择结果，`回车` 下载选中的游戏。
- 下载在后台进行，完成后**停留在界面**显示结果（绿色成功 / 红色失败），可继续搜索下一个。
- `Ctrl-C` 退出；`Esc` 清空输入，输入为空时再按 `Esc` 退出。

### 搜索

使用 Steam 商店搜索接口，语言随查询自适应：中文关键词走简中、否则英文，主语言无结果时自动回退另一种语言，避免中文游戏名搜不到。

## 下载流程

1. `GET github.com/steamtoolsapp/ManifestHub/raw/refs/heads/<id>/<id>.lua`，成功直接保存；分支不存在则回退 Walftech。
2. `POST /gate.php {"action":"challenge"}` 拿到 `challenge` + `difficulty`。
3. 暴力找 `nonce`，使 `sha256(challenge:nonce)` 前缀满足 `difficulty` 个 `0`（PoW）。
4. `POST /gate.php {"action":"redeem", ...}` 用 nonce 换 `token`。
5. `GET /depotbox_lua.php?id=<appid>&token=<token>&format=lua` 下载文件。

请求头必须带 `Referer: https://walftech.com/generator.html`，否则 403。
