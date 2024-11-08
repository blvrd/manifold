package main

import (
	"bufio"
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
	runningCmds  map[int]*exec.Cmd
	terminalSize size
}

type processErrorMsg struct {
	tabIndex int
	err      error
}

type processExitMsg struct {
	tabIndex int
	err      error
}

func (m *Model) runCmd(tabIndex int, commandStrings []string) tea.Cmd {
	return func() tea.Msg {
		if m.runningCmds == nil {
			m.runningCmds = make(map[int]*exec.Cmd)
		}

		if cmd, exists := m.runningCmds[tabIndex]; exists && cmd != nil && cmd.Process != nil {
			m.killProcess(tabIndex)
		}

		cmd := exec.Command(commandStrings[0], commandStrings[1:]...)
		stdout, _ := cmd.StdoutPipe()

		if err := cmd.Start(); err != nil {
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		m.runningCmds[tabIndex] = cmd

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

func (m *Model) killProcess(tabIndex int) error {
	cmd, exists := m.runningCmds[tabIndex]
	if !exists || cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		log.Warnf("Failed to send SIGRTERM to process %d: %v", tabIndex, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		delete(m.runningCmds, tabIndex)
		return err
	case <-time.After(3 * time.Second):
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process %d: %v", tabIndex, err)
		}
		delete(m.runningCmds, tabIndex)
		return fmt.Errorf("process %d killed after timeout", tabIndex)
	}
}

func (m *Model) cleanup() error {
	var errors []error
	done := make(chan bool)

	go func() {
		for tabIndex := range m.runningCmds {
			if err := m.killProcess(tabIndex); err != nil {
				errors = append(errors, fmt.Errorf("tab %d: %w", tabIndex, err))
			}
		}
		done <- true
	}()

	select {
	case <-done:
		if len(errors) > 0 {
			return fmt.Errorf("cleanup errors: %v", errors)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("cleanup timed out after 10 seconds")
	}

	return nil
}

func (m *Model) restartProcess(tabIndex int) tea.Cmd {
	if tabIndex < 0 || tabIndex >= len(m.externalCmds) {
		return nil // might be a good candidate for an assert a la tiger style
	}

	return m.runCmd(tabIndex, m.externalCmds[tabIndex].commandStrings)
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

	m.runningCmds = make(map[int]*exec.Cmd)
	m.externalCmds, err = parseProcfile("./Procfile.dev")

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
			if err := m.cleanup(); err != nil {
				log.Errorf("Cleanup error: %v", err)
			}
			return m, tea.Quit
		case "r":
			return m, m.restartProcess(m.activeTab)
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
	case processErrorMsg:
		if msg.err != nil {
			log.Errorf("Process error on tab %d: %v", msg.tabIndex, msg.err)
			m.TabContent[msg.tabIndex].WriteString(fmt.Sprintf("\nError: %v\n", msg.err))
		}
	case processExitMsg:
		if msg.err != nil {
			log.Warnf("Process on tab %d exited with error: %v", msg.tabIndex, msg.err)
			m.TabContent[msg.tabIndex].WriteString(fmt.Sprintf("\nProcess exited %v\n", msg.err))
		}
		delete(m.runningCmds, msg.tabIndex)
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

func parseProcfile(filepath string) ([]externalCmd, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var commands []externalCmd
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first colon
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue // Skip invalid lines
		}

		name := strings.TrimSpace(parts[0])
		cmdString := strings.TrimSpace(parts[1])

		// Simply split by spaces
		cmdParts := strings.Fields(cmdString)

		if len(cmdParts) > 0 {
			cmd := externalCmd{
				name:           name,
				commandStrings: cmdParts,
			}
			commands = append(commands, cmd)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return commands, nil
}
