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
	pageSize     = 12
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
)

// ---------------------------------------------------------------- 消息

type searchTriggerMsg struct{ query string }

type searchDoneMsg struct {
	query string
	items []searchItem
	err   error
}

type downloadDoneMsg struct {
	label string
	res   downloadResult
	err   error
}

// ---------------------------------------------------------------- 模型

type appModel struct {
	input       textinput.Model
	spinner     spinner.Model
	results     []searchItem
	cursor      int
	width       int
	height      int
	searching   bool
	downloading bool
	status      string
	statusOK    bool
}

func runApp(initial string) error {
	ti := textinput.New()
	ti.Placeholder = "game name or appid, e.g. 730"
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

	m := &appModel{input: ti, spinner: s, statusOK: true}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m *appModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, m.spinner.Tick}
	if m.input.Value() != "" {
		m.searching = true
		cmds = append(cmds, doSearch(m.input.Value()))
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

		switch msg.String() {
		case "esc":
			if m.input.Value() != "" {
				m.input.SetValue("")
				m.results = nil
				m.cursor = 0
				m.searching = false
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
			if m.cursor < len(m.results)-1 {
				m.cursor++
			}
			return m, nil

		case "enter":
			if m.downloading {
				return m, nil
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
			m.results = nil
			return m, tea.Batch(cmd, debounceSearch(m.input.Value()))
		}
		return m, cmd

	case searchTriggerMsg:
		if msg.query != m.input.Value() {
			return m, nil // 过期
		}
		if msg.query == "" {
			m.searching = false
			m.results = nil
			return m, nil
		}
		m.searching = true
		return m, doSearch(msg.query)

	case searchDoneMsg:
		if msg.query != m.input.Value() {
			return m, nil
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

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *appModel) View() string {
	title := titleStyle.Render("Lua4OST · Steam 游戏 Lua 下载器")

	list := m.renderList()

	var statusLine string
	switch {
	case m.downloading:
		statusLine = fmt.Sprintf("%s %s", m.spinner.View(), m.status)
	case m.status != "":
		if m.statusOK {
			statusLine = okStyle.Render(m.status)
		} else {
			statusLine = errStyle.Render(m.status)
		}
	}

	help := helpStyle.Render("Ctrl-C 退出 · ↑/↓ 选择 · 回车 下载 · Esc 清空/退出")

	content := lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		m.input.View(),
		"",
		list,
		statusLine,
		"",
		help,
	)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, boxStyle.Render(content))
}

func (m *appModel) renderList() string {
	if m.searching {
		return fmt.Sprintf("%s 搜索中...", m.spinner.View())
	}
	if len(m.results) == 0 {
		if m.input.Value() == "" {
			return helpStyle.Render("输入游戏名开始搜索（支持中英文）")
		}
		return helpStyle.Render("无结果，试试换个关键词")
	}

	start, end := 0, len(m.results)
	if len(m.results) > pageSize {
		if m.cursor >= start+pageSize {
			start = m.cursor - pageSize + 1
		}
		end = start + pageSize
		if end > len(m.results) {
			end = len(m.results)
			start = end - pageSize
		}
	}

	names := make([]string, 0, end-start)
	maxW := 2
	for i := start; i < end; i++ {
		line := fmt.Sprintf("%s (id=%d)", truncateName(m.results[i].Name, maxNameWidth), m.results[i].ID)
		names = append(names, line)
		if w := lipgloss.Width(line); w > maxW {
			maxW = w
		}
	}
	lineW := maxW + 2

	var b strings.Builder
	for i := start; i < end; i++ {
		if i == m.cursor {
			b.WriteString(selStyle.Width(lineW).Render("▶ " + names[i-start]))
		} else {
			b.WriteString(normStyle.Width(lineW).Render("  " + names[i-start]))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- 命令

func debounceSearch(query string) tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg {
		return searchTriggerMsg{query: query}
	})
}

func doSearch(query string) tea.Cmd {
	return func() tea.Msg {
		items, err := searchApp(query)
		return searchDoneMsg{query: query, items: items, err: err}
	}
}

func doDownloadItem(item searchItem) tea.Cmd {
	appid := strconv.Itoa(item.ID)
	return func() tea.Msg {
		res, err := download(appid)
		return downloadDoneMsg{label: item.Name, res: res, err: err}
	}
}

func doDownloadID(appid string) tea.Cmd {
	return func() tea.Msg {
		res, err := download(appid)
		return downloadDoneMsg{label: "id=" + appid, res: res, err: err}
	}
}

func truncateName(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
