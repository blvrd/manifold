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

type size struct {
	width  int
	height int
}

type Model struct {
	content      strings.Builder
	ready        bool
	viewport     viewport.Model
	Tabs         []string
	TabContent   []strings.Builder
	activeTab    int
	externalCmds []externalCmd
	terminalSize size
}

func (m *Model) runCmd(tabIndex int, commandStrings []string) tea.Cmd {
	return func() tea.Msg {
		// m.content.Reset()
		cmd := exec.Command(commandStrings[0], commandStrings[1:]...)
		log.Debugf("🪚 cmd: %#v", cmd)
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
				m.TabContent[tabIndex].Write(buf[:n])
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
		externalCmd{name: "git status", commandStrings: []string{"git", "status"}},
	}

	var cmds []tea.Cmd
	for i, c := range m.externalCmds {
		m.Tabs = append(m.Tabs, c.name)
		cmds = append(cmds, m.runCmd(i, c.commandStrings))
		m.TabContent = append(m.TabContent, strings.Builder{})
	}
	return tea.Batch(cmds...)
}

var (
	docStyle         = lipgloss.NewStyle().Padding(2)
	highlightColor   = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	borderColor      = lipgloss.AdaptiveColor{Light: "#a0a0a0", Dark: "#3e3e3e"}
	activeTabStyle   = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), true).BorderForeground(highlightColor).Padding(0, 1)
	inactiveTabStyle = lipgloss.NewStyle().Border(lipgloss.HiddenBorder(), true).Padding(0, 1)
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
		isActive := i == m.activeTab
		if isActive {
			style = activeTabStyle
		} else {
			style = inactiveTabStyle
		}
		renderedTabs = append(renderedTabs, style.Render(t))
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
	doc.WriteString(row)
	doc.WriteString("\n")

	doc.WriteString(
		docStyle.Width((m.terminalSize.width - docStyle.GetHorizontalFrameSize())).Border(lipgloss.NormalBorder(), true).BorderForeground(borderColor).Render(m.viewport.View()),
	)
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
			// m.content.Reset()
			// m.content.WriteString("restarting process")
			return m, nil // should runCmd for the current tab
		case "right", "l", "n", "tab":
			m.activeTab = min(m.activeTab+1, len(m.Tabs)-1)
			return m, nil
		case "left", "h", "p", "shift+tab":
			m.activeTab = max(m.activeTab-1, 0)
			return m, nil
		}

	case tickMsg:
		m.viewport.SetContent(m.TabContent[m.activeTab].String())
		m.viewport.GotoBottom()
	case tea.WindowSizeMsg:
		if !m.ready {
			m.viewport = viewport.New(msg.Width-10, msg.Height-20)
			m.viewport.SetContent("Loading...")
			m.ready = true
			break
		}
		m.viewport.Width = msg.Width - 10
		m.viewport.Height = msg.Height - 20
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
