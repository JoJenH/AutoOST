package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	maxNameWidth = 60
	accentColor  = "#8B5CF6"
	bgColor      = "#1F2937"
	fgColor      = "#E5E7EB"
	dimColor     = "#6B7280"
	subColor     = "#9CA3AF"
	okColor      = "#34D399"
	errColor     = "#F87171"
)

var (
	accent = lipgloss.Color(accentColor)

	boxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(dimColor))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(okColor))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(errColor))
	selStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(bgColor)).Background(accent).Bold(true)
	normStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(subColor))

	actionStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(subColor)).Italic(true)
	actionSelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(bgColor)).Background(lipgloss.Color("#06B6D4")).Bold(true)
	warnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color(bgColor)).Background(lipgloss.Color(errColor)).Bold(true)
	noticeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24")).Bold(true)
)

// ---------------------------------------------------------------- 消息

type searchDoneMsg struct {
	query string
	items []appEntry
	err   error
}

type downloadDoneMsg struct {
	label string
	res   downloadResult
	err   error
}

type upgradeDoneMsg struct {
	version  string
	uptodate bool
	err      error
}

// ---------------------------------------------------------------- 模型

type viewMode int

const (
	modeSearch viewMode = iota
	modeLua
	modeLuaContent
)

type appModel struct {
	input       textinput.Model
	spinner     spinner.Model
	apps        []appEntry
	results     []appEntry
	cursor      int
	width       int
	height      int
	downloading bool
	searching   bool
	status      string
	statusOK    bool

	mode           viewMode
	luas           []string
	luaCursor      int
	confirmDelete  string
	luaViewingName string
	luaContent     []string
	contentScroll  int

	toolUpdateAvail bool
	toolAutoCheck   bool
	upgrading       bool
}

func runApp(initial string, apps []appEntry, toolUpdateAvail, toolAutoCheck bool) error {
	ti := textinput.New()
	ti.Placeholder = "game name, e.g. hozy"
	ti.Prompt = "❯ "
	ti.CharLimit = 128
	ti.Width = 56
	ti.PromptStyle = lipgloss.NewStyle().Foreground(accent)
	ti.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(fgColor))
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(dimColor))
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(accent)
	if initial != "" {
		ti.SetValue(initial)
	}
	ti.Focus()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(accent)

	m := &appModel{input: ti, spinner: s, apps: apps, statusOK: true,
		toolUpdateAvail: toolUpdateAvail, toolAutoCheck: toolAutoCheck}
	m.results = searchLocal(initial, apps)
	if initial != "" && len(m.results) == 0 {
		m.searching = true
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m *appModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, m.spinner.Tick}
	if m.searching {
		cmds = append(cmds, doRemoteSearch(m.input.Value()))
	}
	return tea.Batch(cmds...)
}

func (m *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		w := m.width - 14
		if w < 20 {
			w = 20
		}
		if w > 80 {
			w = 80
		}
		m.input.Width = w
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}

		switch m.mode {
		case modeLua:
			return m.updateLuaMode(msg)
		case modeLuaContent:
			return m.updateLuaContentView(msg)
		}

		switch msg.String() {
		case "ctrl+l":
			m.mode = modeLua
			m.luas, _ = listLuaFiles()
			m.luaCursor = 0
			m.confirmDelete = ""
			m.status = ""
			return m, nil

		case "esc":
			if m.input.Value() != "" {
				m.input.SetValue("")
				m.results = nil
				m.cursor = 0
				m.status = ""
				return m, nil
			}
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil

		case "down", "j":
			if m.cursor < m.maxCursor() {
				m.cursor++
			}
			return m, nil

		case "tab":
			// 循环跳到下一个动作行（搜索商店 / OpenSteamTool）
			var actions []int
			if m.input.Value() != "" {
				actions = append(actions, len(m.results))
			}
			if m.toolRowVisible() {
				actions = append(actions, len(m.results)+1)
			}
			next := -1
			for _, r := range actions {
				if r > m.cursor {
					next = r
					break
				}
			}
			if next < 0 && len(actions) > 0 {
				next = actions[0]
			}
			if next >= 0 {
				m.cursor = next
			}
			return m, nil

		case "enter":
			if m.downloading || m.upgrading {
				return m, nil
			}
			// 光标停在 OpenSteamTool 那行 → 检查/更新
			if m.toolRowVisible() && m.cursor == len(m.results)+1 {
				m.upgrading = true
				m.status = "正在检查 OpenSteamTool 更新 ..."
				m.statusOK = true
				return m, doUpdateTool()
			}
			// 光标停在"没有想要的结果？"上 → 手动搜索 Steam 商店。
			if m.cursor >= len(m.results) && m.input.Value() != "" {
				m.searching = true
				m.status = ""
				return m, doRemoteSearch(m.input.Value())
			}
			// 输入框里直接输了数字 appid → 直接下。
			if isNumeric(m.input.Value()) {
				m.downloading = true
				m.status = fmt.Sprintf("下载中: id=%s ...", m.input.Value())
				return m, doDownloadID(m.input.Value())
			}
			if len(m.results) == 0 {
				return m, nil
			}
			item := m.results[m.cursor]
			m.downloading = true
			m.status = fmt.Sprintf("下载中: %s ...", item.Name)
			return m, doDownloadItem(item)
		}

		old := m.input.Value()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if m.input.Value() != old {
			m.cursor = 0
			m.results = searchLocal(m.input.Value(), m.apps)
			m.searching = false
			m.status = ""
			if m.input.Value() != "" && len(m.results) == 0 {
				m.searching = true
				return m, tea.Batch(cmd, doRemoteSearch(m.input.Value()))
			}
			return m, cmd
		}
		return m, cmd

	case searchDoneMsg:
		if msg.query != m.input.Value() {
			return m, nil // 过期
		}
		m.searching = false
		if msg.err != nil {
			m.results = nil
			m.status = fmt.Sprintf("搜索失败: %v", msg.err)
			m.statusOK = false
			return m, nil
		}
		m.results = msg.items
		m.cursor = 0
		if len(m.results) == 0 {
			m.status = fmt.Sprintf("未找到 \"%s\"", msg.query)
			m.statusOK = false
		} else {
			m.status = ""
		}
		return m, nil

	case downloadDoneMsg:
		m.downloading = false
		if msg.err != nil {
			m.status = fmt.Sprintf("%s: 下载失败: %v", msg.label, msg.err)
			m.statusOK = false
		} else {
			m.status = fmt.Sprintf("✓ %s → %s", msg.label, msg.res.String())
			m.statusOK = true
		}
		return m, nil

	case upgradeDoneMsg:
		m.upgrading = false
		switch {
		case msg.err != nil:
			m.status = fmt.Sprintf("OpenSteamTool 更新失败: %v", msg.err)
			m.statusOK = false
		case msg.uptodate:
			m.status = fmt.Sprintf("OpenSteamTool 已是最新 (v%s)", msg.version)
			m.statusOK = true
			m.toolUpdateAvail = false
		default:
			m.status = fmt.Sprintf("✓ OpenSteamTool 已更新到 v%s", msg.version)
			m.statusOK = true
			m.toolUpdateAvail = false
		}
		if m.cursor > m.maxCursor() {
			m.cursor = m.maxCursor()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// toolRowVisible 指示列表下方「OpenSteamTool」那一行是否显示。
func (m *appModel) toolRowVisible() bool {
	return m.toolUpdateAvail || !m.toolAutoCheck || m.upgrading
}

// maxCursor 是可选中的最大光标位置：结果项 + 商店搜索行（+ OpenSteamTool 行）。
func (m *appModel) maxCursor() int {
	n := len(m.results)
	if m.toolRowVisible() {
		n++
	}
	return n
}

// updateLuaMode 处理「已安装 Lua」模式的按键。
func (m *appModel) updateLuaMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeSearch
		m.confirmDelete = ""
		m.status = ""
		return m, nil

	case "up", "k":
		m.confirmDelete = ""
		if m.luaCursor > 0 {
			m.luaCursor--
		}
		return m, nil

	case "down", "j":
		m.confirmDelete = ""
		if m.luaCursor < len(m.luas)-1 {
			m.luaCursor++
		}
		return m, nil

	case "enter":
		if len(m.luas) == 0 {
			return m, nil
		}
		name := m.luas[m.luaCursor]
		lines, err := readLuaFile(name)
		if err != nil {
			m.status = fmt.Sprintf("读取失败: %v", err)
			m.statusOK = false
			return m, nil
		}
		m.luaViewingName = name
		m.luaContent = lines
		m.contentScroll = 0
		m.confirmDelete = ""
		m.mode = modeLuaContent
		return m, nil

	case "d", "delete":
		if len(m.luas) == 0 {
			return m, nil
		}
		name := m.luas[m.luaCursor]
		if m.confirmDelete != name {
			m.confirmDelete = name
			m.status = fmt.Sprintf("再按一次 d 确认删除 %s（Esc 取消）", name)
			m.statusOK = true
			return m, nil
		}
		m.confirmDelete = ""
		n, err := deleteLuaFile(name)
		switch {
		case err != nil:
			m.status = fmt.Sprintf("删除失败: %v", err)
			m.statusOK = false
		case n > 0:
			m.status = fmt.Sprintf("已删除 %s（同时清理 %d 个 manifest）", name, n)
			m.statusOK = true
		default:
			m.status = fmt.Sprintf("已删除 %s", name)
			m.statusOK = true
		}
		m.luas, _ = listLuaFiles()
		if m.luaCursor >= len(m.luas) && m.luaCursor > 0 {
			m.luaCursor = len(m.luas) - 1
		}
		return m, nil
	}
	return m, nil
}

// updateLuaContentView 处理「查看 Lua 内容」模式的按键。
func (m *appModel) updateLuaContentView(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	h := m.luaListHeight()
	switch msg.String() {
	case "esc", "q", "enter":
		m.mode = modeLua
		return m, nil
	case "up", "k":
		if m.contentScroll > 0 {
			m.contentScroll--
		}
	case "down", "j":
		if m.contentScroll < len(m.luaContent)-h {
			m.contentScroll++
		}
	case "pgup":
		m.contentScroll -= h
		if m.contentScroll < 0 {
			m.contentScroll = 0
		}
	case "pgdn":
		m.contentScroll += h
		if m.contentScroll > len(m.luaContent)-h {
			m.contentScroll = len(m.luaContent) - h
		}
		if m.contentScroll < 0 {
			m.contentScroll = 0
		}
	}
	return m, nil
}

func (m *appModel) View() string {
	w := m.width
	if w == 0 {
		w = 80
	}
	boxW := w - 4
	if boxW < 40 {
		boxW = 40
	}
	if boxW > 100 {
		boxW = 100
	}

	switch m.mode {
	case modeLua:
		return m.viewLua(boxW)
	case modeLuaContent:
		return m.viewLuaContent(boxW)
	}

	title := titleStyle.Render("Lua4OST · Steam 游戏 Lua 下载器")
	list := m.renderList()
	action := m.renderAction()
	status := m.statusLine()
	help := helpStyle.Render("Ctrl-C 退出 · ↑/↓ 选择 · 回车 下载 · Tab 商店 · Ctrl+L 已装Lua · Esc 清空")

	content := lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		m.input.View(),
		"",
		list,
		"",
		action,
		m.renderToolAction(),
		"",
		status,
		help,
	)
	box := boxStyle.Width(boxW).Render(fitContent(content, boxW))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// viewLua 渲染「已安装 Lua 文件」视图（与搜索视图保持同高度）。
func (m *appModel) viewLua(boxW int) string {
	title := titleStyle.Render("Lua4OST · 已安装的 Lua 文件")
	header := helpStyle.Render(fmt.Sprintf("共 %d 个 (Steam/config/lua/)", len(m.luas)))
	list := m.renderLuaList()
	hint := helpStyle.Render("回车 查看 · d/Delete 删除")
	status := m.statusLine()
	help := helpStyle.Render("↑/↓ 选择 · 回车 查看 · d 删除 · Esc 返回 · Ctrl-C 退出")

	content := lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		header,
		"",
		list,
		"",
		hint,
		"",
		status,
		help,
	)
	box := boxStyle.Width(boxW).Render(fitContent(content, boxW))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m *appModel) renderLuaList() string {
	h := m.luaListHeight()
	lines := make([]string, h)

	if len(m.luas) == 0 {
		lines[0] = helpStyle.Render("暂无已安装的 Lua 文件")
		return strings.Join(lines, "\n")
	}

	start, end := 0, len(m.luas)
	if len(m.luas) > h {
		if m.luaCursor >= start+h {
			start = m.luaCursor - h + 1
		}
		end = start + h
		if end > len(m.luas) {
			end = len(m.luas)
			start = end - h
		}
	}

	maxW := 2
	for i := start; i < end; i++ {
		if w := lipgloss.Width(m.luas[i]); w > maxW {
			maxW = w
		}
	}
	lineW := maxW + 2

	li := 0
	for i := start; i < end; i++ {
		name := m.luas[i]
		switch {
		case i == m.luaCursor && m.confirmDelete == name:
			lines[li] = warnStyle.Width(lineW).Render("▶ " + name)
		case i == m.luaCursor:
			lines[li] = selStyle.Width(lineW).Render("▶ " + name)
		default:
			lines[li] = normStyle.Width(lineW).Render("  " + name)
		}
		li++
	}
	return strings.Join(lines, "\n")
}

// viewLuaContent 渲染「查看 Lua 内容」视图。
func (m *appModel) viewLuaContent(boxW int) string {
	title := titleStyle.Render(fmt.Sprintf("Lua4OST · %s", m.luaViewingName))

	content := m.renderLuaContent() // 内部会 clamp contentScroll

	total := len(m.luaContent)
	h := m.luaListHeight()
	start := m.contentScroll + 1
	end := m.contentScroll + h
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	header := helpStyle.Render(fmt.Sprintf("第 %d-%d 行 / 共 %d 行", start, end, total))

	hint := helpStyle.Render("PgUp/PgDn 翻页")
	status := m.statusLine()
	help := helpStyle.Render("↑/↓ 滚动 · Esc/q 返回 · Ctrl-C 退出")

	box := boxStyle.Width(boxW).Render(fitContent(lipgloss.JoinVertical(lipgloss.Left,
		title, "", header, "", content, "", hint, "", status, help,
	), boxW))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

// fitContent 先把内容按盒子内宽截断，避免 boxStyle.Width 触发自动换行、
// 导致盒子高度随文本长度变化。
func fitContent(content string, boxW int) string {
	innerW := boxW - 6 // 边框 2 + 左右 padding 各 2
	if innerW < 1 {
		innerW = 1
	}
	return lipgloss.NewStyle().MaxWidth(innerW).Render(content)
}

func (m *appModel) renderLuaContent() string {
	h := m.luaListHeight()
	total := len(m.luaContent)
	if m.contentScroll > total-h {
		m.contentScroll = total - h
	}
	if m.contentScroll < 0 {
		m.contentScroll = 0
	}
	lines := make([]string, h)
	for i := 0; i < h; i++ {
		idx := m.contentScroll + i
		if idx < total {
			lines[i] = strings.ReplaceAll(m.luaContent[idx], "\t", "    ")
		}
	}
	return strings.Join(lines, "\n")
}

// listHeight 根据终端高度给出固定行数（不随结果条数变化）。
func (m *appModel) listHeight() int {
	h := m.height
	if h == 0 {
		h = 24
	}
	h -= 14
	if h < 4 {
		h = 4
	}
	return h
}

// luaListHeight 是 lua 视图的列表高度（比搜索视图多 1 行，保证整体盒子同高）。
func (m *appModel) luaListHeight() int {
	return m.listHeight() + 1
}

func (m *appModel) renderList() string {
	h := m.listHeight()
	lines := make([]string, h)

	if m.searching {
		lines[0] = fmt.Sprintf("%s 搜索中...", m.spinner.View())
		return strings.Join(lines, "\n")
	}
	if len(m.results) == 0 {
		if m.input.Value() == "" {
			lines[0] = helpStyle.Render("输入游戏名开始搜索（本地离线搜索）")
		} else {
			lines[0] = helpStyle.Render("本地无结果")
		}
		return strings.Join(lines, "\n")
	}

	start, end := 0, len(m.results)
	if len(m.results) > h {
		if m.cursor >= start+h {
			start = m.cursor - h + 1
		}
		end = start + h
		if end > len(m.results) {
			end = len(m.results)
			start = end - h
		}
	}

	names := make([]string, 0, end-start)
	maxW := 2
	for i := start; i < end; i++ {
		line := m.resultLine(i)
		names = append(names, line)
		if w := lipgloss.Width(line); w > maxW {
			maxW = w
		}
	}
	lineW := maxW + 2

	li := 0
	for i := start; i < end; i++ {
		if i == m.cursor {
			lines[li] = selStyle.Width(lineW).Render("▶ " + names[i-start])
		} else {
			lines[li] = normStyle.Width(lineW).Render("  " + names[i-start])
		}
		li++
	}
	return strings.Join(lines, "\n")
}

func (m *appModel) renderAction() string {
	if m.input.Value() == "" {
		return ""
	}
	action := "没有想要的结果？→ 搜索 Steam 商店"
	if m.cursor == len(m.results) {
		return actionSelStyle.Render("▶ " + action)
	}
	return actionStyle.Render("  " + action)
}

// renderToolAction 渲染 OpenSteamTool 那一行（固定在列表下方）。
func (m *appModel) renderToolAction() string {
	switch {
	case m.upgrading:
		return noticeStyle.Render("  OpenSteamTool 更新中...")
	case m.toolUpdateAvail:
		label := "⚠ OpenSteamTool 有新版本"
		if m.cursor == len(m.results)+1 {
			return actionSelStyle.Render("▶ " + label)
		}
		return noticeStyle.Render("  " + label)
	case !m.toolAutoCheck:
		// 关闭了自动检查：保留手动入口
		label := "检查 OpenSteamTool 更新"
		if m.cursor == len(m.results)+1 {
			return actionSelStyle.Render("▶ " + label)
		}
		return actionStyle.Render("  " + label)
	}
	return ""
}

func (m *appModel) statusLine() string {
	switch {
	case m.downloading || m.upgrading:
		return fmt.Sprintf("%s %s", m.spinner.View(), m.status)
	case m.status != "":
		if m.statusOK {
			return okStyle.Render(m.status)
		}
		return errStyle.Render(m.status)
	}
	return ""
}

func (m *appModel) resultLine(i int) string {
	e := m.results[i]
	name := truncateName(e.Name, maxNameWidth)
	if e.Type != "game" {
		return fmt.Sprintf("%s (id=%d, %s)", name, e.ID, e.Type)
	}
	return fmt.Sprintf("%s (id=%d)", name, e.ID)
}

// ---------------------------------------------------------------- 命令

func doRemoteSearch(query string) tea.Cmd {
	return func() tea.Msg {
		items, err := searchRemote(query)
		return searchDoneMsg{query: query, items: items, err: err}
	}
}

func doDownloadItem(item appEntry) tea.Cmd {
	appid := strconv.Itoa(item.ID)
	return func() tea.Msg {
		res, err := download(appid, item.Name)
		return downloadDoneMsg{label: item.Name, res: res, err: err}
	}
}

func doDownloadID(appid string) tea.Cmd {
	return func() tea.Msg {
		res, err := download(appid, "")
		return downloadDoneMsg{label: "id=" + appid, res: res, err: err}
	}
}

// doUpdateTool 在 TUI 中检查并（如有）安装 OpenSteamTool 最新版。
// 静默执行，不打印以免破坏界面。
func doUpdateTool() tea.Cmd {
	return func() tea.Msg {
		if isSteamRunning() {
			return upgradeDoneMsg{err: fmt.Errorf("Steam 正在运行，请先退出 Steam 再更新")}
		}
		installed := installedToolVersion(steamDir)
		latest, err := latestTag(15 * time.Second)
		if err != nil {
			return upgradeDoneMsg{err: err}
		}
		if installed == latest {
			return upgradeDoneMsg{version: latest, uptodate: true}
		}
		ver, err := doInstallTool(steamDir)
		return upgradeDoneMsg{version: ver, err: err}
	}
}

func truncateName(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
