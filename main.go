package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
)

type externalCmd struct {
	name           string
	commandStrings []string
}

type Model struct {
	content      strings.Builder
	ready        bool
	viewport     viewport.Model
	Tabs         []string
	TabContent   []strings.Builder
	activeTab    int
	externalCmds []externalCmd
}

func (m *Model) runCmd(commandStrings []string) tea.Cmd {
	return func() tea.Msg {
		// m.content.Reset()
    cmd := exec.Command(commandStrings[0], commandStrings[1:]...)
		stdout, _ := cmd.StdoutPipe()

		if err := cmd.Start(); err != nil {
			return tea.Quit()
		}

		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := stdout.Read(buf)
				if err != nil {
					break
				}
				m.content.Write(buf[:n])
			}
		}()

		return nil
	}
}

func (m *Model) Init() tea.Cmd {
	logFile, err := os.OpenFile("debug.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)

	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		os.Exit(1)
	}

	log.SetOutput(logFile)
	log.SetLevel(log.DebugLevel)
	log.Info("application started")

	m.externalCmds = []externalCmd{
		externalCmd{name: "web", commandStrings: []string{"bash", "fake_process.sh"}},
	}

	var cmds []tea.Cmd
	for _, c := range m.externalCmds {
		m.Tabs = append(m.Tabs, c.name)
		cmds = append(cmds, m.runCmd(c.commandStrings))
		m.TabContent = append(m.TabContent, strings.Builder{})
	}
	return tea.Batch(cmds...)
}

func tabBorderWithBottom(left, middle, right string) lipgloss.Border {
	border := lipgloss.RoundedBorder()
	border.BottomLeft = left
	border.Bottom = middle
	border.BottomRight = right
	return border
}

var (
	inactiveTabBorder = tabBorderWithBottom("┴", "─", "┴")
	activeTabBorder   = tabBorderWithBottom("┘", " ", "└")
	docStyle          = lipgloss.NewStyle().Padding(1, 2, 1, 2)
	highlightColor    = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	inactiveTabStyle  = lipgloss.NewStyle().Border(inactiveTabBorder, true).BorderForeground(highlightColor).Padding(0, 1)
	activeTabStyle    = inactiveTabStyle.Border(activeTabBorder, true)
	windowStyle       = lipgloss.NewStyle().BorderForeground(highlightColor).Padding(2, 0).Align(lipgloss.Center).Border(lipgloss.NormalBorder()).UnsetBorderTop()
)

func (m *Model) View() string {
	if !m.ready {
		return "Loading..."
	}
	// return m.viewport.View()
	doc := strings.Builder{}

	var renderedTabs []string

	for i, t := range m.Tabs {
		var style lipgloss.Style
		isFirst, isLast, isActive := i == 0, i == len(m.Tabs)-1, i == m.activeTab
		if isActive {
			style = activeTabStyle
		} else {
			style = inactiveTabStyle
		}
		border, _, _, _, _ := style.GetBorder()
		if isFirst && isActive {
			border.BottomLeft = "│"
		} else if isFirst && !isActive {
			border.BottomLeft = "├"
		} else if isLast && isActive {
			border.BottomRight = "│"
		} else if isLast && !isActive {
			border.BottomRight = "┤"
		}
		style = style.Border(border)
		renderedTabs = append(renderedTabs, style.Render(t))
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
	doc.WriteString(row)
	doc.WriteString("\n")
	doc.WriteString(windowStyle.Width((lipgloss.Width(row) - windowStyle.GetHorizontalFrameSize())).Render(m.viewport.View()))
	return docStyle.Render(doc.String())
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "r":
			m.content.Reset()
			m.content.WriteString("restarting process")
			return m, nil // should runCmd for the current tab
		case "right", "l", "n", "tab":
			m.activeTab = min(m.activeTab+1, len(m.Tabs)-1)
			return m, nil
		case "left", "h", "p", "shift+tab":
			m.activeTab = max(m.activeTab-1, 0)
			return m, nil
		}

	case tickMsg:
		m.viewport.SetContent(m.content.String())
		// m.viewport.GotoBottom()

	case tea.WindowSizeMsg:
		if !m.ready {
      m.viewport = viewport.New(msg.Width, msg.Height-10)
      m.viewport.YPosition = 5
      m.viewport.HighPerformanceRendering = false
      m.viewport.SetContent("Loading...")
      m.ready = true
			break
		}
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 10
	}

	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

type tickMsg time.Time

func main() {
	cmd := tea.NewProgram(&Model{}, tea.WithAltScreen())

	go func() {
		for c := range time.Tick(100 * time.Millisecond) {
			cmd.Send(tickMsg(c))
		}
	}()

	if _, err := cmd.Run(); err != nil {
		fmt.Printf("Uh oh: %v\n", err)
		os.Exit(1)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
