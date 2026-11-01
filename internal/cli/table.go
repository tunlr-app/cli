package cli

import (
	"context"
	"io"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	ltable "charm.land/lipgloss/v2/table"
)

// writeTable 输出没有边框的表格：列宽按显示宽度计算（带颜色的单元格也对齐），列间两格，行尾不留空格。
func writeTable(w io.Writer, header []string, rows [][]string) {
	last := len(header) - 1
	t := ltable.New().Headers(header...).Rows(rows...).Wrap(false).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).BorderHeader(false).BorderColumn(false).
		StyleFunc(func(_, col int) lipgloss.Style {
			if col < last {
				return lipgloss.NewStyle().PaddingRight(2)
			}
			return lipgloss.NewStyle()
		})
	for line := range strings.Lines(t.String()) {
		io.WriteString(w, strings.TrimRight(line, " \n")+"\n")
	}
}

// rowChoice 是在表格中选一行的提问（prompter.ChooseRow）。
type rowChoice struct {
	// title 是问题，note 是标题下的一行说明（可以为空）。
	title, note string
	header      []string
	rows        [][]string
	// labels 是选定后留在屏幕上的各行的称呼，为 nil 时用整行。
	labels []string
	// initial 是光标起始的行。
	initial int
	// empty 是没有行时代替表格显示的一句话。
	empty string
	// keys 是 Enter、Esc 以外的按键，按下时 ChooseRow 返回光标所在的行（没有行时为 -1）和这个键。
	keys []rowKey
}

// rowKey 是选择表格的行时另外的按键，help 是帮助行中的说明。
type rowKey struct{ key, help string }

// 选择表格的行时的样式，与 huh 的 ThemeBase16 一致：左边的竖线、青色的标题、绿色的所在行。
var (
	pickerBox   = lipgloss.NewStyle().BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(lipgloss.Color("8")).PaddingLeft(1)
	pickerTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	pickerNote  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// ChooseRow 用 bubbles 的 table 在原地显示表格（不进入备用屏幕），上下箭头移动，Enter 选定，Esc、q、Ctrl-C 取消。
// 结束后屏幕上留下：选定时为标题和所选行的称呼，取消时为不带光标的表格，按了 keys 中的键时为标题和这个键的说明。
func (p huhPrompter) ChooseRow(ctx context.Context, c rowChoice) (int, string, error) {
	m := newRowPicker(c)
	if _, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(p.a.stdin), tea.WithOutput(p.a.stderr)).Run(); err != nil {
		return 0, "", err
	}
	if m.aborted {
		return 0, "", huh.ErrUserAborted
	}
	return m.chosen, m.key, nil
}

// rowPicker 是 ChooseRow 的 Bubble Tea 模型。第一列是光标（>），其余是 rowChoice 的列。
type rowPicker struct {
	c       rowChoice
	table   table.Model
	chosen  int
	key     string
	aborted bool
	done    bool
}

func newRowPicker(c rowChoice) *rowPicker {
	cols := []table.Column{{Width: 1}}
	for i, h := range c.header {
		w := lipgloss.Width(h)
		for _, r := range c.rows {
			w = max(w, lipgloss.Width(r[i]))
		}
		cols = append(cols, table.Column{Title: h, Width: w})
	}
	km := table.DefaultKeyMap()
	// 只留箭头和翻页键：字母留给 rowChoice.keys。
	km.LineUp.SetKeys("up")
	km.LineDown.SetKeys("down")
	km.PageUp.SetKeys("pgup")
	km.PageDown.SetKeys("pgdown")
	km.HalfPageUp.SetEnabled(false)
	km.HalfPageDown.SetEnabled(false)
	km.GotoTop.SetKeys("home")
	km.GotoBottom.SetKeys("end")
	width := 0
	for _, col := range cols {
		width += col.Width + 2
	}
	m := &rowPicker{c: c, table: table.New(
		table.WithColumns(cols), table.WithFocused(true), table.WithKeyMap(km), table.WithWidth(width),
		table.WithHeight(min(len(c.rows), maxListRows)+1),
		table.WithStyles(table.Styles{
			Header:   lipgloss.NewStyle().Bold(true).PaddingRight(2),
			Cell:     lipgloss.NewStyle().PaddingRight(2),
			Selected: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		}))}
	m.mark()
	m.table.SetCursor(c.initial)
	m.mark()
	return m
}

// mark 把光标（>）画在所在的行。不单独上色：所在的行整行是绿色，单元格内的颜色会打断它。
func (m *rowPicker) mark() {
	rows := make([]table.Row, len(m.c.rows))
	for i, r := range m.c.rows {
		cursor := ""
		if i == m.table.Cursor() {
			cursor = ">"
		}
		rows[i] = append(table.Row{cursor}, r...)
	}
	m.table.SetRows(rows)
}

func (m *rowPicker) Init() tea.Cmd { return nil }

func (m *rowPicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch s := k.String(); s {
		case "enter":
			if len(m.c.rows) == 0 {
				return m, nil
			}
			m.chosen, m.done = m.table.Cursor(), true
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.aborted, m.done = true, true
			return m, tea.Quit
		default:
			for _, x := range m.c.keys {
				if s == x.key {
					m.chosen, m.key, m.done = -1, x.key, true
					if len(m.c.rows) > 0 {
						m.chosen = m.table.Cursor()
					}
					return m, tea.Quit
				}
			}
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.mark()
	return m, cmd
}

func (m *rowPicker) View() tea.View {
	var b strings.Builder
	b.WriteString(pickerTitle.Render(m.c.title))
	switch {
	case m.done && m.key != "":
		for _, x := range m.c.keys {
			if x.key == m.key {
				b.WriteString(": " + x.help)
			}
		}
		return tea.NewView(pickerBox.Render(b.String()) + "\n")
	case m.done && !m.aborted:
		label := strings.Join(m.c.rows[m.chosen], "  ")
		if m.chosen < len(m.c.labels) {
			label = m.c.labels[m.chosen]
		}
		b.WriteString(": " + label)
		return tea.NewView(pickerBox.Render(b.String()) + "\n")
	}
	if m.c.note != "" {
		b.WriteString("\n" + pickerNote.Render(m.c.note))
	}
	b.WriteString("\n")
	switch {
	case len(m.c.rows) == 0:
		b.WriteString(pickerNote.Render(m.c.empty))
	case m.done:
		var t strings.Builder
		writeTable(&t, m.c.header, m.c.rows)
		b.WriteString(strings.TrimSuffix(t.String(), "\n"))
	default:
		b.WriteString(m.table.View())
	}
	out := pickerBox.Render(b.String()) + "\n"
	if !m.done {
		help := []string{"↑ up", "↓ down"}
		if len(m.c.rows) > 0 {
			help = append(help, "enter choose")
		}
		for _, x := range m.c.keys {
			help = append(help, x.key+" "+x.help)
		}
		out += "\n" + styleDim.Render(strings.Join(append(help, "esc back"), " • ")) + "\n"
	}
	return tea.NewView(out)
}
