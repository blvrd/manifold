package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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
	maxBytes int
	buffer   []byte
	mu       sync.Mutex
}

func newBufferedOutput(maxBytes int) *bufferedOutput {
	return &bufferedOutput{
		maxBytes: maxBytes,
		buffer:   make([]byte, 0, maxBytes),
	}
}

// Write implements io.Writer. Once the size of the buffer exceeds the buffer's
// maxBytes, earlier content is truncated
func (b *bufferedOutput) Write(data []byte) (int, error) {
	if len(data) > b.maxBytes {
		return 0, fmt.Errorf("write size %d exceeds buffer max size %d", len(data), b.maxBytes)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if (len(b.buffer) + len(data)) > b.maxBytes {
		spillover := len(b.buffer) + len(data) - b.maxBytes

		if spillover < len(b.buffer) {
			b.buffer = b.buffer[spillover:]
		} else {
			b.buffer = b.buffer[:0]
		}
		return len(data), nil
	}
	b.buffer = append(b.buffer, data...)

	return len(data), nil
}

func (b *bufferedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buffer)
}

func (b *bufferedOutput) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffer = b.buffer[:0:b.maxBytes]
}

type TabStatus int

const (
	StatusNone TabStatus = iota
	StatusStreaming
	StatusSuccess
	StatusError
	StatusQuitting
)

type Tab interface {
	Name() string
	Content() string
	Clear()
	SetStatus(TabStatus)
	Status() TabStatus
	YOffset() int
	SetYOffset(int)
	Following() bool
	SetFollowing(bool)
	Write([]byte) (int, error)
	CommandStrings() []string
	RunningCmd() *exec.Cmd
	SetRunningCmd(*exec.Cmd)
}

type ProcessTab struct {
	name           string
	yOffset        int
	following      bool
	status         TabStatus
	buffer         *bufferedOutput
	commandStrings []string
	runningCmd     *exec.Cmd
	mu             sync.Mutex
}

func NewProcessTab(name string, commandStrings []string) *ProcessTab {
	return &ProcessTab{
		name:           name,
		commandStrings: commandStrings,
		buffer:         newBufferedOutput(1000),
	}
}

func (p *ProcessTab) Name() string             { return p.name }
func (p *ProcessTab) Content() string          { return p.buffer.String() }
func (p *ProcessTab) Clear()                   { p.buffer.Clear() }
func (p *ProcessTab) YOffset() int             { return p.yOffset }
func (p *ProcessTab) SetYOffset(y int)         { p.yOffset = y }
func (p *ProcessTab) Following() bool          { return p.following }
func (p *ProcessTab) SetFollowing(f bool)      { p.following = f }
func (p *ProcessTab) CommandStrings() []string { return p.commandStrings }

func (p *ProcessTab) Status() TabStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *ProcessTab) SetStatus(s TabStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = s
}

func (p *ProcessTab) RunningCmd() *exec.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runningCmd
}
func (p *ProcessTab) SetRunningCmd(cmd *exec.Cmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runningCmd = cmd
}

func (p *ProcessTab) Write(b []byte) (int, error) {
	p.SetStatus(StatusStreaming)
	return p.buffer.Write(b)
}

type HelpTab struct {
	name    string
	yOffset int
	content string
}

func (h *HelpTab) Name() string                { return h.name }
func (h *HelpTab) Content() string             { return h.content }
func (h *HelpTab) Status() TabStatus           { return StatusNone }
func (h *HelpTab) SetStatus(TabStatus)         { /* noop */ }
func (h *HelpTab) YOffset() int                { return h.yOffset }
func (h *HelpTab) SetYOffset(y int)            { h.yOffset = y }
func (h *HelpTab) Following() bool             { return false }
func (h *HelpTab) SetFollowing(bool)           { /* noop */ }
func (h *HelpTab) CommandStrings() []string    { return nil }
func (h *HelpTab) RunningCmd() *exec.Cmd       { return nil }
func (h *HelpTab) SetRunningCmd(cmd *exec.Cmd) {}
func (h *HelpTab) Clear()                      {}

func (h *HelpTab) Write(b []byte) (int, error) {
	h.content = string(b)
	return len(b), nil
}

type Model struct {
	ready        bool
	viewport     viewport.Model
	tabs         []Tab
	activeTab    int
	terminalSize size
	scrollbar    tea.Model
	keys         keyMap
	help         help.Model
	procfilePath string
	quitting     bool
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
		log.Debug("starting command execution", "tab", tabIndex, "cmd", commandStrings)

		tab := m.tabs[tabIndex]
		if cmd := tab.RunningCmd(); cmd != nil && cmd.Process != nil {
			err := m.killProcess(tab)
			if err != nil {
				return processErrorMsg{tabIndex: tabIndex, err: err}
			}
		}

		cmd := exec.Command(commandStrings[0], commandStrings[1:]...)
		// set up a new process group to be identified later
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		ptmx, tty, err := pty.Open()
		if err != nil {
			log.Error("failed to open pty", "error", err)
		}

		cmd.Stdout = tty
		cmd.Stderr = tty
		cmd.Stdin = tty

		if err := cmd.Start(); err != nil {
			log.Error("failed to start command", "error", err)
			tab.SetStatus(StatusError)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		tty.Close()

		log.Debug("process started successfully", "tab", tabIndex, "pid", cmd.Process.Pid)

		tab.SetRunningCmd(cmd)
		tab.SetStatus(StatusStreaming)

		errChan := make(chan processErrorMsg)

		go func() {
			defer ptmx.Close()
			scanner := bufio.NewScanner(ptmx)
			for scanner.Scan() {
				line := scanner.Bytes()
				// log.Debug("pty received", "tab", tabIndex, "content", string(line))
				_, err := tab.Write(append(line, '\n'))
				if err != nil {
					errChan <- processErrorMsg{err: err, tabIndex: tabIndex}
					return
				}
			}

			if err := scanner.Err(); err != nil {
				// log.Error("pty scanner error", "tab", tabIndex, "error", err)
				_, err := tab.Write([]byte(fmt.Sprintf("\nPTY error: %v\n", err)))
				if err != nil {
					errChan <- processErrorMsg{err: err, tabIndex: tabIndex}
					return
				}
			}
			// log.Debug("pty scanner finished", "tab", tabIndex)

			if err := cmd.Wait(); err != nil {
				log.Error("process exited with error", "tab", tabIndex, "error", err)
				tab.SetRunningCmd(nil)
				_, err := tab.Write([]byte(fmt.Sprintf("\nProcess exited with error: %v\n", err)))
				if err != nil {
					errChan <- processErrorMsg{err: err, tabIndex: tabIndex}
					return
				}
				tab.SetStatus(StatusError)
			} else {
				log.Debug("process exited successfully", "tab", tabIndex)
				tab.SetRunningCmd(nil)
				tab.SetStatus(StatusSuccess)
			}
		}()

		return func() tea.Msg {
			select {
			case errMsg := <-errChan:
				return errMsg
			case <-time.After(100 * time.Millisecond):
				return nil
			}
		}
	}
}

func (m *Model) killProcess(tab Tab) error {
	cmd := tab.RunningCmd()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	start := time.Now()
	log.Debug("killing process", "tab", tab.Name(), "pid", pid)

	// Send SIGINT to the process group
	if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
		log.Debug("failed to kill process group", "error", err)
		return fmt.Errorf("failed to kill process %s: %v", tab.Name(), err)
	}

	// Wait for process to actually terminate
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			// Process is gone
			log.Debug("process terminated", "pid", pid)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	log.Debug("process failed to terminate gracefully, forcing kill", "pid", pid, "duration", time.Since(start))
	return syscall.Kill(-pid, syscall.SIGKILL)
}

type cleanupDoneMsg struct {
	err error
}

func (m *Model) cleanup() tea.Msg {
	var errors []error
	done := make(chan bool)

	go func() {
		start := time.Now()

		log.Debug("starting cleanup", "process_count", len(m.tabs))

		for _, tab := range m.tabs {
			if err := m.killProcess(tab); err != nil {
				errors = append(errors, fmt.Errorf("tab %s: %w", tab.Name(), err))
			}
		}

		log.Debug("cleanup completed", "duration", time.Since(start), "error_count", len(errors))
		done <- true
	}()

	select {
	case <-done:
		if len(errors) > 0 {
			return cleanupDoneMsg{err: fmt.Errorf("cleanup errors: %v", errors)}
		}
	case <-time.After(5 * time.Second):
		return cleanupDoneMsg{err: fmt.Errorf("cleanup timed out after 5 seconds")}
	}

	return cleanupDoneMsg{}
}

func (m *Model) restartProcess(tabIndex int) tea.Cmd {
	if pt, ok := m.tabs[tabIndex].(*ProcessTab); ok {
		return m.runCmd(tabIndex, pt.commandStrings)
	}

	return nil
}

func (m *Model) Init() tea.Cmd {
	if os.Getenv("DEBUG") != "" {
		logFile, err := os.OpenFile("debug.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Printf("Error opening log file: %v\n", err)
			os.Exit(1)
		}

		log.SetOutput(logFile)
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetOutput(io.Discard)
		log.SetLevel(log.FatalLevel)
	}
	log.Info("application started")

	procfile, err := parseProcfile(m.procfilePath)
	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		os.Exit(1)
	}

	var cmds []tea.Cmd
	for _, cmd := range procfile {
		tab := NewProcessTab(cmd.name, cmd.commandStrings)
		index := len(m.tabs)
		m.tabs = append(m.tabs, tab)
		cmds = append(cmds, m.runCmd(index, tab.commandStrings))
	}

	// this could use some refactoring
	helpTab := HelpTab{
		name:    "help",
		yOffset: 0,
		content: m.getHelpContent(),
	}
	m.tabs = append(m.tabs, &helpTab)

	return tea.Batch(cmds...)
}

func (m *Model) currentTab() Tab {
	return m.tabs[m.activeTab]
}

func (m *Model) getHelpContent() string {
	var content strings.Builder

	content.WriteString("Manifold\n")
	content.WriteString("========\n\n")
	content.WriteString("Manifold is a simple, Procfile-based process manager. For each process defined in your Procfile, Manifold will run the process in its own tab.\n")
	content.WriteString("Each tab has a little colored dot that indicates its status. Blue means the process is still running, green means the process exited with a zero exit code, and red means it exited with a non-zero exit code.\n\n")

	content.WriteString("Keyboard shortcuts: \n\n")

	for _, section := range m.keys.FullHelp() {
		for _, binding := range section {
			keys := binding.Help().Key
			desc := binding.Help().Desc
			content.WriteString(fmt.Sprintf("%-12s: %s\n", keys, desc))
		}
		content.WriteString("\n")
	}

	return content.String()
}

var (
	docStyle         = lipgloss.NewStyle().Padding(0).Border(lipgloss.NormalBorder(), true).BorderForeground(borderColor)
	windowStyle      = lipgloss.NewStyle().Padding(1, 2)
	highlightColor   = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	borderColor      = lipgloss.AdaptiveColor{Light: "#a0a0a0", Dark: "#3e3e3e"}
	lightText        = lipgloss.AdaptiveColor{Light: "#a0a0a0", Dark: "#9f9f9f"}
	streamingColor   = lipgloss.AdaptiveColor{Light: "#3498db", Dark: "#3498db"}
	errorColor       = lipgloss.AdaptiveColor{Light: "#e74c3c", Dark: "#e74c3c"}
	successColor     = lipgloss.AdaptiveColor{Light: "#2ecc71", Dark: "#2ecc71"}
	dimmedColor      = lipgloss.AdaptiveColor{Light: "#c9c9c9", Dark: "#8a8a8a"}
	activeTabStyle   = lipgloss.NewStyle().MarginRight(2).Bold(true)
	inactiveTabStyle = lipgloss.NewStyle().MarginRight(2).Foreground(lightText)
)

func (m *Model) View() string {
	if !m.ready {
		return "Loading..."
	}
	doc := strings.Builder{}

	var renderedTabs []string

	for i, t := range m.tabs {
		var tabNameStyle lipgloss.Style
		isActive := i == m.activeTab
		if isActive {
			tabNameStyle = activeTabStyle
		} else {
			tabNameStyle = inactiveTabStyle
		}

		status := t.Status()
		if status != StatusNone {
			var dotColor lipgloss.AdaptiveColor
			switch status {
			case StatusStreaming:
				dotColor = streamingColor
			case StatusSuccess:
				dotColor = successColor
			case StatusError:
				dotColor = errorColor
			case StatusQuitting:
				dotColor = dimmedColor
			}

			statusIndicator := lipgloss.NewStyle().MarginRight(1).Foreground(dotColor).Render("⏺")
			renderedTabs = append(renderedTabs, lipgloss.JoinHorizontal(lipgloss.Left, statusIndicator, tabNameStyle.Render(t.Name())))
		} else {
			renderedTabs = append(renderedTabs, tabNameStyle.Render(t.Name()))
		}
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
	doc.WriteString(row)
	doc.WriteString("\n")

	if len(m.currentTab().CommandStrings()) > 0 {
		doc.WriteString(lipgloss.NewStyle().Foreground(lightText).Render(fmt.Sprintf("Running: %s", strings.Join(m.currentTab().CommandStrings(), " "))))
	}
	doc.WriteString("\n")
	if m.viewport.TotalLineCount() > m.viewport.VisibleLineCount() {
		doc.WriteString(
			lipgloss.JoinHorizontal(
				lipgloss.Center,
				docStyle.Width(m.viewport.Width).Render(m.viewport.View()),
				m.scrollbar.View(),
			),
		)
	} else {
		doc.WriteString(
			docStyle.Width(m.viewport.Width).Render(m.viewport.View()),
		)
	}
	helpView := m.help.View(m.keys)
	doc.WriteString("\n")
	doc.WriteString(helpView)

	window := windowStyle.Render(doc.String())
	if m.quitting {
		overlayBoxStyle := lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).Width(40).Height(4).Padding(1)
		overlayContent := overlayBoxStyle.Render("Shutting down processes gracefully...")

		// Place the overlay in the center of the screen (sorta)
		return PlaceOverlay(m.terminalSize.width/2-overlayBoxStyle.GetWidth()/2, m.terminalSize.height/2-10, overlayContent, windowStyle.Faint(true).Render(doc.String()), false)
	}
	return window
}

type TabDirection int

const (
	TabPrevious TabDirection = -1
	TabNext     TabDirection = 1
)

func (m *Model) switchTab(direction TabDirection) tea.Cmd {
	// save the current tab's offset
	m.currentTab().SetYOffset(m.viewport.YOffset)

	// calculate the new tab index with wrapping
	numTabs := len(m.tabs)
	newIndex := (m.activeTab + int(direction) + numTabs) % numTabs
	m.activeTab = newIndex

	// update the viewport content before restoring the tab's y-offset
	m.viewport.SetContent(m.tabs[m.activeTab].Content())

	// restore the tab's y-offset, careful not do go over the max offset
	maxOffset := max(0, m.viewport.TotalLineCount()-m.viewport.Height)
	m.viewport.YOffset = clamp(m.currentTab().YOffset(), 0, maxOffset)

	// immediately update the scrollbar
	var cmd tea.Cmd
	m.scrollbar, cmd = m.scrollbar.Update(m.viewport)

	return cmd
}

func (m *Model) switchToLastTab() tea.Cmd {
	// save the current tab's offset
	m.currentTab().SetYOffset(m.viewport.YOffset)
	m.activeTab = len(m.tabs) - 1
	// update the viewport content before restoring the tab's y-offset
	m.viewport.SetContent(m.currentTab().Content())
	// restore the tab's y-offset, careful not do go over the max offset
	maxOffset := max(0, m.viewport.TotalLineCount()-m.viewport.Height)
	m.viewport.YOffset = clamp(m.currentTab().YOffset(), 0, maxOffset)
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
	Clear          key.Binding
	Follow         key.Binding
	Unfollow       key.Binding
	ScrollTop      key.Binding
	ScrollBottom   key.Binding
	Help           key.Binding
	Quit           key.Binding
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.RestartProcess, k.Clear, k.Follow, k.Unfollow},
		{k.Up, k.Down, k.Left, k.Right, k.ScrollTop, k.ScrollBottom},
		{k.Help, k.Quit},
	}
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k/ctrl+u", "scroll up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j/ctrl+d", "scroll down"),
	),
	Left: key.NewBinding(
		key.WithKeys("left", "h"),
		key.WithHelp("←/h", "prev tab"),
	),
	Right: key.NewBinding(
		key.WithKeys("right", "l"),
		key.WithHelp("→/l", "next tab"),
	),
	RestartProcess: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "restart process"),
	),
	Clear: key.NewBinding(
		key.WithKeys("c"),
		key.WithHelp("c", "clear"),
	),
	Follow: key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "follow output"),
	),
	Unfollow: key.NewBinding(
		key.WithKeys("u"),
		key.WithHelp("u", "unfollow output"),
	),
	ScrollTop: key.NewBinding(
		key.WithKeys("t"),
		key.WithHelp("t", "scroll to top"),
	),
	ScrollBottom: key.NewBinding(
		key.WithKeys("b"),
		key.WithHelp("b", "scroll to bottom"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "open help"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "esc", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	if m.quitting {
		switch msg := msg.(type) {
		case cleanupDoneMsg:
			if msg.err != nil {
				log.Errorf("Cleanup error: %v", msg.err)
			}
			return m, tea.Quit
		}
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			m.quitting = true
			for _, t := range m.tabs {
				t.SetStatus(StatusQuitting)
			}
			return m, m.cleanup
		case key.Matches(msg, keys.Help):
			// m.help.ShowAll = !m.help.ShowAll
			return m, m.switchToLastTab()
		case key.Matches(msg, keys.RestartProcess):
			return m, m.restartProcess(m.activeTab)
		case key.Matches(msg, keys.Clear):
			m.currentTab().Clear()
			return m, nil
		case key.Matches(msg, keys.Right):
			return m, m.switchTab(TabNext)
		case key.Matches(msg, keys.Left):
			return m, m.switchTab(TabPrevious)
		case key.Matches(msg, keys.Follow):
			m.currentTab().SetFollowing(true)
			return m, nil
		case key.Matches(msg, keys.Unfollow):
			m.currentTab().SetFollowing(false)
			return m, nil
		case key.Matches(msg, keys.ScrollTop):
			m.viewport.GotoTop()
			m.currentTab().SetFollowing(false)
			return m, nil
		case key.Matches(msg, keys.ScrollBottom):
			m.viewport.GotoBottom()
			return m, nil
		}
	case tickMsg:
		m.viewport.SetContent(m.tabs[m.activeTab].Content())
		if m.currentTab().Following() {
			m.viewport.GotoBottom()
		}
	case tea.WindowSizeMsg:
		contentWidth := msg.Width - windowStyle.GetHorizontalFrameSize() - docStyle.GetHorizontalFrameSize()
		contentHeight := msg.Height - windowStyle.GetVerticalFrameSize() - docStyle.GetVerticalFrameSize() - 4 // -4 for tab row and help line

		if !m.ready {
			m.terminalSize = size{width: msg.Width, height: msg.Height}

			m.viewport = viewport.New(contentWidth, contentHeight)
			m.scrollbar = scrollbar.NewVertical(
				scrollbar.WithThumbStyle(lipgloss.NewStyle().Foreground(highlightColor).SetString("┃")),
				scrollbar.WithTrackStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).SetString("│")),
			)
			m.scrollbar, cmd = m.scrollbar.Update(scrollbar.HeightMsg(contentHeight))
			cmds = append(cmds, cmd)
			m.viewport.SetContent("Loading...")
			m.ready = true
			break
		}

		m.viewport.Width = contentWidth
		m.viewport.Height = contentHeight

		m.scrollbar, cmd = m.scrollbar.Update(scrollbar.HeightMsg(contentHeight))
		cmds = append(cmds, cmd)
	case processErrorMsg:
		if msg.err != nil {
			log.Errorf("Process error on tab %d: %v", msg.tabIndex, msg.err)
			_, err := m.tabs[msg.tabIndex].Write([]byte(fmt.Sprintf("\nError: %v\n", msg.err)))
			if err != nil {
				// handle this in a better way
				panic(err)
			}
			m.tabs[msg.tabIndex].SetStatus(StatusError)
		}
	case processExitMsg:
		if msg.err != nil {
			log.Warnf("Process on tab %d exited with error: %v", msg.tabIndex, msg.err)
			_, err := m.tabs[msg.tabIndex].Write([]byte(fmt.Sprintf("\nProcess exited %v\n", msg.err)))
			if err != nil {
				// handle this in a better way
				panic(err)
			}
		}
		m.tabs[msg.tabIndex].SetRunningCmd(nil)
	}

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)
	m.scrollbar, cmd = m.scrollbar.Update(m.viewport)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

type tickMsg time.Time

func main() {
	procfilePath := flag.String("f", "Procfile.dev", "path to Procfile")
	flag.Parse()

	if _, err := os.Stat(*procfilePath); os.IsNotExist(err) {
		if *procfilePath == "Procfile.dev" {
			fmt.Fprintf(os.Stderr, "Error: No Procfile.dev found in current directory.\n\n")
			fmt.Fprintf(os.Stderr, "Please either:\n")
			fmt.Fprintf(os.Stderr, "  1. Create a Procfile.dev in the current directory\n")
			fmt.Fprintf(os.Stderr, "  2. Specify a different Procfile with -p\n\n")
			fmt.Fprintf(os.Stderr, "Example: manifold -p path/to/Procfile\n")
			os.Exit(1)
		} else {
			fmt.Fprintf(os.Stderr, "Error: Procfile not found at %s\n", *procfilePath)
			os.Exit(1)
		}
	}

	cmd := tea.NewProgram(
		&Model{
			keys:         keys,
			help:         help.New(),
			procfilePath: *procfilePath,
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
