package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"
)

type Model struct {
	content  strings.Builder
	ready    bool
	viewport viewport.Model
}

func (m *Model) runCmd() tea.Msg {
	cmd := exec.Command("bash", "fake_process.sh")
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

func (m *Model) Init() tea.Cmd {
  logFile, err := os.OpenFile("debug.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)

	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		os.Exit(1)
	}

	log.SetOutput(logFile)
	log.SetLevel(log.DebugLevel)
	log.Info("application started")
	return m.runCmd
}

func (m *Model) View() string {
	if !m.ready {
		return "Loading..."
	}
	return m.viewport.View()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
    switch msg.String() {
    case "ctrl+c", "q", "esc":
			return m, tea.Quit
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
