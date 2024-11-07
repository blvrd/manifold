package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
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
	return m.runCmd
}

func (m *Model) View() string {
	if !m.ready {
		return "Loading..."
	}
	return m.viewport.View()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if k := msg.String(); k == "ctrl+c" || k == "q" || k == "esc" {
			return m, tea.Quit
		}

	case tickMsg:
		m.viewport.SetContent(m.content.String())
		m.viewport.GotoBottom()

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

	return m, nil
}

type tickMsg time.Time

func main() {
	cmd := tea.NewProgram(&Model{})

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
