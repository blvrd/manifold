package main

import (
	"bufio"
	"flag"
	"fmt"
	"golang.org/x/sys/unix"
	// "golang.org/x/term"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

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
	height int //nolint:all
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

func (b *bufferedOutput) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.buffer)+len(p) > b.maxBytes {
		excess := len(b.buffer) + len(p) - b.maxBytes
		if excess < len(b.buffer) {
			b.buffer = b.buffer[excess:]
		} else {
			b.buffer = b.buffer[:0]
		}
	}

	b.buffer = append(b.buffer, p...)
	return len(p), nil
}

func (b *bufferedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buffer)
}

func (b *bufferedOutput) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffer = b.buffer[:0]
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
	WriteToPty([]byte) error
}

type ProcessTab struct {
	name           string
	yOffset        int
	following      bool
	status         TabStatus
	buffer         *bufferedOutput
	commandStrings []string
	pty            *os.File
	termios        *unix.Termios
	origTermios    *unix.Termios
}

func NewProcessTab(name string, commandStrings []string) *ProcessTab {
	return &ProcessTab{
		name:           name,
		commandStrings: commandStrings,
		buffer:         newBufferedOutput(1024 * 1024), // 1MB
	}
}

func (p *ProcessTab) Name() string             { return p.name }
func (p *ProcessTab) Content() string          { return p.buffer.String() }
func (p *ProcessTab) Clear()                   { p.buffer.Clear() }
func (p *ProcessTab) Status() TabStatus        { return p.status }
func (p *ProcessTab) SetStatus(s TabStatus)    { p.status = s }
func (p *ProcessTab) YOffset() int             { return p.yOffset }
func (p *ProcessTab) SetYOffset(y int)         { p.yOffset = y }
func (p *ProcessTab) Following() bool          { return p.following }
func (p *ProcessTab) SetFollowing(f bool)      { p.following = f }
func (p *ProcessTab) CommandStrings() []string { return p.commandStrings }

func (p *ProcessTab) Write(b []byte) (int, error) {
	p.status = StatusStreaming
	return p.buffer.Write(b)
}

func (p *ProcessTab) WriteToPty(b []byte) error {
	if p.pty == nil {
		return fmt.Errorf("no PTY available")
	}
	log.Debug("writing to pty", "bytes", b, "string", string(b))
	n, err := p.pty.Write(b)
	log.Debug("wrote to pty", "bytes_written", n)
	return err
}

type HelpTab struct {
	name    string
	yOffset int
	content string
}

func (h *HelpTab) Name() string              { return h.name }
func (h *HelpTab) Content() string           { return h.content }
func (h *HelpTab) Status() TabStatus         { return StatusNone }
func (h *HelpTab) SetStatus(TabStatus)       { /* noop */ }
func (h *HelpTab) YOffset() int              { return h.yOffset }
func (h *HelpTab) SetYOffset(y int)          { h.yOffset = y }
func (h *HelpTab) Following() bool           { return false }
func (h *HelpTab) SetFollowing(bool)         { /* noop */ }
func (h *HelpTab) CommandStrings() []string  { return nil }
func (h *HelpTab) Clear()                    {}
func (h *HelpTab) WriteToPty(b []byte) error { return nil }

func (h *HelpTab) Write(b []byte) (int, error) {
	h.content = string(b)
	return len(b), nil
}

type Model struct {
	ready        bool
	viewport     viewport.Model
	tabs         []Tab
	activeTab    int
	runningCmds  map[int]*exec.Cmd
	cmdsMutex    sync.Mutex
	terminalSize size
	scrollbar    tea.Model
	keys         keyMap
	help         help.Model
	procfilePath string
	quitting     bool
	interactive  bool
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
		m.cmdsMutex.Lock()
		if m.runningCmds == nil {
			m.runningCmds = make(map[int]*exec.Cmd)
		}

		if cmd, exists := m.runningCmds[tabIndex]; exists && cmd != nil && cmd.Process != nil {
			m.cmdsMutex.Unlock()
			err := m.killProcess(tabIndex)
			if err != nil {
				return processErrorMsg{tabIndex: tabIndex, err: err}
			}
			m.cmdsMutex.Lock()
		}

		cmd := exec.Command(commandStrings[0], commandStrings[1:]...)
		// set up a new process group to be identified later
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		ptmx, tty, err := pty.Open()
		if err != nil {
			log.Error("failed to open pty", "error", err)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		// Get current settings for both master and slave
		masterTermios, err := unix.IoctlGetTermios(int(ptmx.Fd()), unix.TIOCGETA)
		if err != nil {
			log.Error("failed to get master termios", "error", err)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}
		slaveTermios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TIOCGETA)
		if err != nil {
			log.Error("failed to get slave termios", "error", err)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		// Save original settings
		origTermios := *slaveTermios

		// Configure slave PTY
		slaveTermios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		slaveTermios.Oflag &^= unix.OPOST
		slaveTermios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		slaveTermios.Cflag &^= unix.CSIZE | unix.PARENB
		slaveTermios.Cflag |= unix.CS8

		// Configure master PTY
		masterTermios.Iflag |= unix.ICRNL
		masterTermios.Oflag |= unix.ONLCR
		masterTermios.Lflag |= unix.ICANON | unix.ECHO

		if err := unix.IoctlSetTermios(int(tty.Fd()), unix.TIOCSETA, slaveTermios); err != nil {
			log.Error("failed to set slave termios", "error", err)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		if err := unix.IoctlSetTermios(int(ptmx.Fd()), unix.TIOCSETA, masterTermios); err != nil {
			log.Error("failed to set master termios", "error", err)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		cmd.Stdout = tty
		cmd.Stderr = tty
		cmd.Stdin = tty

		if err := cmd.Start(); err != nil {
			log.Error("failed to start command", "error", err)
			m.cmdsMutex.Unlock()
			m.tabs[tabIndex].SetStatus(StatusError)
			return processErrorMsg{tabIndex: tabIndex, err: err}
		}

		// Store the states in the ProcessTab for cleanup
		if pt, ok := m.tabs[tabIndex].(*ProcessTab); ok {
			pt.pty = ptmx
      pt.termios = masterTermios
      pt.origTermios = &origTermios
		}

		// close our end of the tty - the process has its own file descriptor now
		tty.Close()

		log.Debug("process started successfully", "tab", tabIndex, "pid", cmd.Process.Pid)

		m.runningCmds[tabIndex] = cmd
		m.tabs[tabIndex].SetStatus(StatusStreaming)
		m.cmdsMutex.Unlock()

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			// remove the pty from the tab when we're done
			defer func() {
				if pt, ok := m.tabs[tabIndex].(*ProcessTab); ok {
					if pt != nil {
						if pt.termios != nil {
							unix.IoctlSetTermios(int(pt.pty.Fd()), unix.TIOCSETA, pt.termios)
						}
						pt.pty.Close()
						pt.pty = nil
					}
				}
			}()

			log.Debug("starting pty read loop", "tab", tabIndex)
			buf := make([]byte, 32*1024)

			for {
				n, err := ptmx.Read(buf)
				if err != nil {
					if err != io.EOF {
						log.Error("pty read error", "error", err)
					}
					log.Debug("pty read loop ended", "error", err, "tab", tabIndex)
					break
				}

				if n > 0 {
					log.Debug("read from pty",
						"bytes", buf[:n],
						"string", string(buf[:n]),
						"length", n,
						"tab", tabIndex)
					_, err = m.tabs[tabIndex].Write(buf[:n])
					if err != nil {
						log.Error("tab write error", "error", err)
						break
					}
				}
			}
		}()

		go func() {
			defer func() {
				<-readDone // wait for read loop to finish
				log.Debug("command wait finished", "tab", tabIndex)
			}()
			if err := cmd.Wait(); err != nil {
				log.Error("process exited with error", "tab", tabIndex, "error", err)
				m.cmdsMutex.Lock()
				delete(m.runningCmds, tabIndex)
				m.cmdsMutex.Unlock()
				// _, err := m.tabs[tabIndex].Write([]byte(fmt.Sprintf("\nProcess exited with error: %v\n", err)))
				// if err != nil {
				// 	// handle this in a better way
				// 	panic(err)
				// }
				m.tabs[tabIndex].SetStatus(StatusError)
			} else {
				log.Debug("process exited successfully", "tab", tabIndex)
				m.cmdsMutex.Lock()
				delete(m.runningCmds, tabIndex)
				m.cmdsMutex.Unlock()
				m.tabs[tabIndex].SetStatus(StatusSuccess)
			}
		}()

		return nil
	}
}

func (m *Model) killProcess(tabIndex int) error {
	m.cmdsMutex.Lock()
	cmd, exists := m.runningCmds[tabIndex]
	if !exists || cmd == nil || cmd.Process == nil {
		m.cmdsMutex.Unlock()
		return nil
	}
	pid := cmd.Process.Pid
	m.cmdsMutex.Unlock()

	start := time.Now()
	log.Debug("killing process", "tab", tabIndex, "pid", pid)

	// Send SIGINT to the process group
	if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
		log.Debug("failed to kill process group", "error", err)
		return fmt.Errorf("failed to kill process %d: %v", tabIndex, err)
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
		m.cmdsMutex.Lock()
		tabs := make([]int, 0, len(m.runningCmds))
		for tabIndex := range m.runningCmds {
			tabs = append(tabs, tabIndex)
		}
		m.cmdsMutex.Unlock()
		start := time.Now()

		log.Debug("starting cleanup", "process_count", len(tabs))

		for _, tabIndex := range tabs {
			if err := m.killProcess(tabIndex); err != nil {
				errors = append(errors, fmt.Errorf("tab %d: %w", tabIndex, err))
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

func (m *Model) handlePtyInput(msg tea.KeyMsg) tea.Cmd {
	// this shouldn't need the type switch every time, right??
	if pt, ok := m.tabs[m.activeTab].(*ProcessTab); ok {

		var bytes []byte
		switch msg.Type {
		case tea.KeyRunes:
			bytes = []byte(msg.String())
		case tea.KeyEnter:
			bytes = []byte{'\r'}
		case tea.KeyBackspace:
			bytes = []byte{'\x7f'}
    case tea.KeyLeft:
        bytes = []byte{'\x1b', '[', 'D'}
    case tea.KeyRight:
        bytes = []byte{'\x1b', '[', 'C'}
    case tea.KeyUp:
        bytes = []byte{'\x1b', '[', 'A'}
    case tea.KeyDown:
        bytes = []byte{'\x1b', '[', 'B'}
		case tea.KeyCtrlC:
			bytes = []byte{'\x03'}
		case tea.KeyCtrlD:
			bytes = []byte{'\x04'}
		case tea.KeyCtrlU:
			bytes = []byte{'\x15'}
		case tea.KeyCtrlW:
			bytes = []byte{'\x17'}
		case tea.KeyTab:
			bytes = []byte{'\t'}
		case tea.KeySpace:
			bytes = []byte{' '}
		default:
			log.Debug("unhandled key type", "type", msg.Type)
			return nil
		}

		log.Debug("sending to pty",
			"key_type", msg.Type,
			"bytes", bytes,
			"hex", fmt.Sprintf("%x", bytes))

		if err := pt.WriteToPty(bytes); err != nil {
			log.Error("failed to write to pty", "error", err)
		}
	}

	return nil
}

func (m *Model) setTerminalSize(width, height int) {
	m.cmdsMutex.Lock()
	defer m.cmdsMutex.Unlock()

	for i, tab := range m.tabs {
		if pt, ok := tab.(*ProcessTab); ok && pt.pty != nil {
			if cmd, exists := m.runningCmds[i]; exists && cmd != nil && cmd.Process != nil {
				ws := &struct {
					Row    uint16
					Col    uint16
					Xpixel uint16
					Ypixel uint16
				}{
					Row:    uint16(height),
					Col:    uint16(width),
					Xpixel: 0,
					Ypixel: 0,
				}
				syscall.Syscall(
					syscall.SYS_IOCTL,
					pt.pty.Fd(),
					syscall.TIOCSWINSZ,
					uintptr(unsafe.Pointer(ws)),
				)
			}
		}
	}
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

	m.runningCmds = make(map[int]*exec.Cmd)
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

	if m.interactive {
		doc.WriteString(lipgloss.NewStyle().Render(" (interactive)"))
	}

	doc.WriteString("\n")

	var style lipgloss.Style
	if m.interactive {
		style = docStyle.BorderForeground(streamingColor)
	} else {
		style = docStyle
	}
	if m.viewport.TotalLineCount() > m.viewport.VisibleLineCount() {
		doc.WriteString(
			lipgloss.JoinHorizontal(
				lipgloss.Center,
				style.Width(m.viewport.Width).Render(m.viewport.View()),
				m.scrollbar.View(),
			),
		)
	} else {
		doc.WriteString(
			style.Width(m.viewport.Width).Render(m.viewport.View()),
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
	Interact       key.Binding
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
	Interact: key.NewBinding(
		key.WithKeys("i"),
		key.WithHelp("i", "interact with process"),
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

	if m.interactive {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch {
			case msg.Type == tea.KeyEsc:
				m.interactive = false
				return m, nil
			default:
				log.Debug("handling interactive input", "key", msg.String())
				return m, m.handlePtyInput(msg)
			}
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
		case key.Matches(msg, keys.Interact):
			m.interactive = true
			return m, nil
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

		m.setTerminalSize(contentWidth, contentHeight)
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
