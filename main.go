package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/blvrd/manifold/help"
	"github.com/blvrd/manifold/scrollbar"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/creack/pty"
)

type externalCmd struct {
	name           string
	commandStrings []string
}

type size struct {
	width  int
	height int
}

type bufferedOutput struct {
	maxLines int
	lines    []string
	mu       sync.Mutex
}

func newBufferedOutput(maxLines int) *bufferedOutput {
	return &bufferedOutput{
		maxLines: maxLines,
		lines:    make([]string, 0, maxLines),
	}
}

func (b *bufferedOutput) Write(p []byte, tabIndex int, m *Model) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	text := string(p)
	lines := strings.Split(text, "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}

		if len(b.lines) >= b.maxLines {
			b.lines = b.lines[1:]
		}
		b.lines = append(b.lines, line)
	}

	m.Tabs[tabIndex].LastOutput = time.Now()
	m.Tabs[tabIndex].Status = ProcessStreaming

	return len(p), nil
}

func (b *bufferedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return strings.Join(b.lines, "\n")
}

type Tab struct {
	Name       string
	YOffset    int
	Following  bool
	Status     ProcessStatus
	LastOutput time.Time
}

type Model struct {
	content      strings.Builder
	ready        bool
	viewport     viewport.Model
	Tabs         []*Tab
	TabContent   []*bufferedOutput
	activeTab    int
	externalCmds []externalCmd
	runningCmds  map[int]*exec.Cmd
	cmdsMutex    sync.Mutex
	terminalSize size
	scrollbar    tea.Model
	keys         keyMap
	help         help.Model
}

type processErrorMsg struct {
	tabIndex int
	err      error
}

type processExitMsg struct {
	tabIndex int
	err      error
}

type ProcessStatus int

const (
	ProcessStreaming ProcessStatus = iota
	ProcessPaused
	ProcessError
)

func (m *Model) runCmd(tabIndex int, commandStrings []string) tea.Cmd {
	return func() tea.Msg {
		log.Debug("starting command execution", "tab", tabIndex, "cmd", commandStrings)
		m.cmdsMutex.Lock()
		if m.runningCmds == nil {
			m.runningCmds = make(map[int]*exec.Cmd)
		}

		if cmd, exists := m.runningCmds[tabIndex]; exists && cmd != nil && cmd.Process != nil {
			m.cmdsMutex.Unlock()
			m.killProcess(tabIndex)
			m.cmdsMutex.Lock()
		}

		cmd := exec.Command(commandStrings[0], commandStrings[1:]...)

		cmd.Env = append(os.Environ(),
			"FORCE_COLOR=1",
			"COLORTERM=truecolor",
			"TERM=xterm-256color",
		)

		ptmx, tty, err := pty.Open()
		if err != nil {
			log.Error("failed to open pty", "error", err)
		}

		cmd.Stdout = tty
		cmd.Stderr = tty
		cmd.Stdin = tty

		if err := cmd.Start(); err != nil {
			log.Error("failed to start command", "error", err)
			m.cmdsMutex.Unlock()
			m.Tabs[tabIndex].Status = ProcessError
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		tty.Close()

		log.Debug("process started successfully", "tab", tabIndex, "pid", cmd.Process.Pid)

		m.runningCmds[tabIndex] = cmd
		m.Tabs[tabIndex].Status = ProcessStreaming
		m.cmdsMutex.Unlock()

		go func() {
			defer ptmx.Close()
			scanner := bufio.NewScanner(ptmx)
			for scanner.Scan() {
				line := scanner.Bytes()
				log.Debug("pty received", "tab", tabIndex, "content", string(line))
				m.TabContent[tabIndex].Write(append(line, '\n'), tabIndex, m)
			}

			if err := scanner.Err(); err != nil {
				log.Error("pty scanner error", "tab", tabIndex, "error", err)
				m.TabContent[tabIndex].Write([]byte(fmt.Sprintf("\nPTY error: %v\n", err)), tabIndex, m)
			}
			log.Debug("pty scanner finished", "tab", tabIndex)

			if err := cmd.Wait(); err != nil {
				log.Error("process exited with error", "tab", tabIndex, "error", err)
				m.cmdsMutex.Lock()
				delete(m.runningCmds, tabIndex)
				m.cmdsMutex.Unlock()
				m.TabContent[tabIndex].Write([]byte(fmt.Sprintf("\nProcess exited with error: %v\n", err)), tabIndex, m)
				m.Tabs[tabIndex].Status = ProcessError
			} else {
				log.Debug("process exited successfully", "tab", tabIndex)
				m.cmdsMutex.Lock()
				delete(m.runningCmds, tabIndex)
				m.cmdsMutex.Unlock()
			}
		}()

		return nil
	}
}

func (m *Model) killProcess(tabIndex int) error {
	m.cmdsMutex.Lock()
	cmd, exists := m.runningCmds[tabIndex]
	m.cmdsMutex.Unlock()

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
		m.cmdsMutex.Lock()
		delete(m.runningCmds, tabIndex)
		m.cmdsMutex.Unlock()
		return err
	case <-time.After(3 * time.Second):
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process %d: %v", tabIndex, err)
		}
		<-done
		m.cmdsMutex.Lock()
		delete(m.runningCmds, tabIndex)
		m.cmdsMutex.Unlock()
		return fmt.Errorf("process %d killed after timeout", tabIndex)
	}
}

func (m *Model) cleanup() error {
	var errors []error
	done := make(chan bool)

	go func() {
		m.cmdsMutex.Lock()
		tabs := make([]int, 0, len(m.runningCmds))
		for tabIndex := range m.runningCmds {
			tabs = append(tabs, tabIndex)
		}
		m.cmdsMutex.Unlock()

		for _, tabIndex := range tabs {
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
		m.Tabs = append(m.Tabs, &Tab{Name: c.name, YOffset: 0})
		cmds = append(cmds, m.runCmd(i, c.commandStrings))
		m.TabContent = append(m.TabContent, newBufferedOutput(10000))
	}
	return tea.Batch(cmds...)
}

func (m *Model) currentTab() *Tab {
	return m.Tabs[m.activeTab]
}

var (
	docStyle         = lipgloss.NewStyle().Padding(0)
	windowStyle      = lipgloss.NewStyle().Padding(2)
	highlightColor   = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	borderColor      = lipgloss.AdaptiveColor{Light: "#a0a0a0", Dark: "#3e3e3e"}
	lightText        = lipgloss.AdaptiveColor{Light: "#a0a0a0", Dark: "#9f9f9f"}
	activeTabStyle   = lipgloss.NewStyle().MarginRight(2).Bold(true)
	inactiveTabStyle = lipgloss.NewStyle().MarginRight(2).Foreground(lightText)
)

func (m *Model) View() string {
	if !m.ready {
		return "Loading..."
	}
	doc := strings.Builder{}

	var renderedTabs []string

	for i, t := range m.Tabs {
		var tabNameStyle lipgloss.Style
		isActive := i == m.activeTab
		if isActive {
			tabNameStyle = activeTabStyle
		} else {
			tabNameStyle = inactiveTabStyle
		}

		var dotColor lipgloss.Color
		switch t.Status {
		case ProcessStreaming:
			dotColor = lipgloss.Color("#2ecc71")
		case ProcessPaused:
			dotColor = lipgloss.Color("#f1c40f")
		case ProcessError:
			dotColor = lipgloss.Color("#e74c3c")
		}

		statusIndicator := lipgloss.NewStyle().MarginRight(1).Foreground(dotColor).Render("⏺")
		renderedTabs = append(renderedTabs, lipgloss.JoinHorizontal(lipgloss.Left, statusIndicator, tabNameStyle.Render(t.Name)))
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
	doc.WriteString(row)
	doc.WriteString("\n")

	doc.WriteString(lipgloss.NewStyle().Foreground(lightText).Render(fmt.Sprintf("Running: %s", strings.Join(m.externalCmds[m.activeTab].commandStrings, " "))))
	doc.WriteString("\n")
	if m.viewport.TotalLineCount() > m.viewport.VisibleLineCount() {
		doc.WriteString(
			lipgloss.JoinHorizontal(lipgloss.Center, docStyle.Width((m.terminalSize.width-docStyle.GetHorizontalFrameSize())).Border(lipgloss.NormalBorder(), true).BorderForeground(borderColor).Render(m.viewport.View()), m.scrollbar.View()),
		)
	} else {
		doc.WriteString(
			docStyle.Width((m.terminalSize.width - docStyle.GetHorizontalFrameSize())).Border(lipgloss.NormalBorder(), true).BorderForeground(borderColor).Render(m.viewport.View()),
		)
	}
	helpView := m.help.View(m.keys)
	doc.WriteString("\n")
	doc.WriteString(helpView)
	return windowStyle.Render(doc.String())
}

type TabDirection int

const (
	TabPrevious TabDirection = -1
	TabNext     TabDirection = 1
)

func (m *Model) switchTab(direction TabDirection) tea.Cmd {
	// save the current tab's offset
	m.currentTab().YOffset = m.viewport.YOffset

	// calculate the new tab index with wrapping
	numTabs := len(m.Tabs)
	newIndex := (m.activeTab + int(direction) + numTabs) % numTabs
	m.activeTab = newIndex

	// update the viewport content before restoring the tab's y-offset
	m.viewport.SetContent(m.TabContent[m.activeTab].String())

	// restore the tab's y-offset, careful not do go over the max offset
	maxOffset := max(0, m.viewport.TotalLineCount()-m.viewport.Height)
	m.viewport.YOffset = clamp(m.currentTab().YOffset, 0, maxOffset)

	// immediately update the scrollbar
	var cmd tea.Cmd
	m.scrollbar, cmd = m.scrollbar.Update(m.viewport)

	return cmd
}

type keyMap struct {
	Up             key.Binding
	Down           key.Binding
	Left           key.Binding
	Right          key.Binding
	RestartProcess key.Binding
	Follow         key.Binding
	Unfollow       key.Binding
	ScrollTop      key.Binding
	Help           key.Binding
	Quit           key.Binding
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.RestartProcess, k.ScrollTop},
		{k.Follow, k.Unfollow},
		{k.Up, k.Down},
		{k.Left, k.Right},
		{k.Help, k.Quit},
	}
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "move up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "move down"),
	),
	Left: key.NewBinding(
		key.WithKeys("left", "h"),
		key.WithHelp("←/h", "move left"),
	),
	Right: key.NewBinding(
		key.WithKeys("right", "l"),
		key.WithHelp("→/l", "move right"),
	),
	RestartProcess: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "restart process"),
	),
	Follow: key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "follow"),
	),
	Unfollow: key.NewBinding(
		key.WithKeys("u"),
		key.WithHelp("u", "unfollow"),
	),
	ScrollTop: key.NewBinding(
		key.WithKeys("t"),
		key.WithHelp("t", "scroll to top"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "toggle help"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "esc", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			if err := m.cleanup(); err != nil {
				log.Errorf("Cleanup error: %v", err)
			}
			return m, tea.Quit
		case key.Matches(msg, keys.Help):
			m.help.ShowAll = !m.help.ShowAll
		case key.Matches(msg, keys.RestartProcess):
			return m, m.restartProcess(m.activeTab)
		case key.Matches(msg, keys.Right):
			return m, m.switchTab(TabNext)
		case key.Matches(msg, keys.Left):
			return m, m.switchTab(TabPrevious)
		case key.Matches(msg, keys.Follow):
			m.currentTab().Following = true
			return m, nil
		case key.Matches(msg, keys.Unfollow):
			m.currentTab().Following = false
			return m, nil
		case key.Matches(msg, keys.ScrollTop):
			m.viewport.GotoTop()
			m.currentTab().Following = false
			return m, nil
		}
	case tickMsg:
		m.viewport.SetContent(m.TabContent[m.activeTab].String())
		if m.currentTab().Following {
			m.viewport.GotoBottom()
		}

		// check for paused processes
		now := time.Now()

		for i, tab := range m.Tabs {
			m.cmdsMutex.Lock()
			cmd, running := m.runningCmds[i]
			m.cmdsMutex.Unlock()

			if running && cmd != nil && cmd.Process != nil {
				if tab.Status != ProcessError && now.Sub(tab.LastOutput) > 3*time.Second {
					tab.Status = ProcessPaused
				}
			}
		}
	case tea.WindowSizeMsg:
		if !m.ready {
			m.viewport = viewport.New(msg.Width-20, msg.Height-20)
			m.scrollbar = scrollbar.NewVertical(
				scrollbar.WithThumbStyle(lipgloss.NewStyle().Foreground(highlightColor).SetString("┃")),
				scrollbar.WithTrackStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).SetString("│")),
			)
			m.scrollbar, cmd = m.scrollbar.Update(scrollbar.HeightMsg(m.viewport.Height))
			cmds = append(cmds, cmd)
			m.viewport.SetContent("Loading...")
			m.ready = true
			break
		}
		m.viewport.Width = msg.Width - 20
		m.viewport.Height = msg.Height - 20
		m.scrollbar, cmd = m.scrollbar.Update(scrollbar.HeightMsg(m.viewport.Height))
		cmds = append(cmds, cmd)
	case processErrorMsg:
		if msg.err != nil {
			log.Errorf("Process error on tab %d: %v", msg.tabIndex, msg.err)
			m.TabContent[msg.tabIndex].Write(
				[]byte(fmt.Sprintf("\nError: %v\n", msg.err)),
				m.activeTab,
				m,
			)
			m.Tabs[msg.tabIndex].Status = ProcessError
		}
	case processExitMsg:
		if msg.err != nil {
			log.Warnf("Process on tab %d exited with error: %v", msg.tabIndex, msg.err)
			m.TabContent[msg.tabIndex].Write(
				[]byte(fmt.Sprintf("\nProcess exited %v\n", msg.err)),
				m.activeTab,
				m,
			)
		}
		delete(m.runningCmds, msg.tabIndex)
	}

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)
	m.scrollbar, cmd = m.scrollbar.Update(m.viewport)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

type tickMsg time.Time

func main() {
	cmd := tea.NewProgram(
		&Model{
			keys: keys,
			help: help.New(),
		},
		tea.WithAltScreen(),
	)

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

func clamp(v, low, high int) int {
	if high < low {
		low, high = high, low
	}
	return min(high, max(low, v))
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
