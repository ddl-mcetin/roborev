package tui

import (
	"fmt"
	"strings"
)

// renderAgentPickerView draws the modal that lets the user pick an agent and
// re-enqueue the currently-highlighted review job with that agent.
func (m model) renderAgentPickerView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Re-run with different agent"))
	b.WriteString("\n")
	if m.agentPickerJobInfo != "" {
		b.WriteString("  " + m.agentPickerJobInfo)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if len(m.agentPickerAgents) == 0 && m.agentPickerErr != "" {
		// Refusal state — render the error and the cancel hint only.
		b.WriteString(fmt.Sprintf("\nWarning: %s\n\n", m.agentPickerErr))
		helpRows := [][]helpItem{
			{{"esc/q", "close"}},
		}
		b.WriteString(renderHelpTable(helpRows, m.width))
		return b.String()
	}

	for i, name := range m.agentPickerAgents {
		prefix := "  "
		line := name
		if i == m.agentPickerIdx {
			prefix = "> "
			line = selectedStyle.Render(name)
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}

	if m.agentPickerErr != "" {
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("Warning: %s", m.agentPickerErr))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	helpRows := [][]helpItem{
		{{"↑/↓ or j/k", "navigate"}, {"enter", "re-enqueue with selected agent"}, {"esc/q", "cancel"}},
	}
	b.WriteString(renderHelpTable(helpRows, m.width))

	return b.String()
}
