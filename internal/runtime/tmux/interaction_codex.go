package tmux

import (
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Codex approval overlay
// ---------------------------------------------------------------------------
//
// Codex asks for approval with an overlay that replaces its composer at the
// bottom of the pane (codex-rs/tui/src/bottom_pane/approval_overlay.rs). As of
// codex-cli 0.162.1 it looks like this (80 columns):
//
//	  Would you like to run the following command?
//
//	  Environment: local
//
//	  Reason: May I write the requested two-line file to
//	  /tmp/codex-approval-b955b7.txt? The current environment is read-only.
//
//	  $ printf "%s\n" one two > /tmp/codex-approval-b955b7.txt
//
//
//	› 1. Yes, proceed (y)
//	  2. Yes, and don't ask again for commands that start with `printf "%s\n" one
//	     two > /tmp/codex-approval-b955b7.txt` (p)
//	  3. No, and tell Codex what to do differently (esc)
//
//	  Press enter to confirm or esc to cancel
//
// Unlike Claude's dialog, a body sits between the question and the menu, each
// label ends with its shortcut key, and the title and footer wrap on a narrow
// pane. Every overlay row is indented two columns except the highlighted
// option, so a row that starts in the first column above the menu (the
// "› …" user message, a "• Ran …" history line) means the question above it
// is history. The highlighted option also starts with "› ", Codex's
// ready-prompt prefix, so an overlay gc does not recognize reads as an idle
// composer. Digit keys select an option, as in Claude's menu. See
// testdata/codex-approval for real captures.

var (
	// codexApprovalQuestionRe matches the overlay titles that ask for
	// approval of a command, a network host, a file edit, a permission grant,
	// or input to a running terminal.
	codexApprovalQuestionRe = regexp.MustCompile(`^(Would you like to (run the following command|make the following edits|grant these permissions|send input to .+)|Do you want to approve network access to .+)\?$`)

	// codexNetworkHostRe extracts the host from a network approval title.
	codexNetworkHostRe = regexp.MustCompile(`^Do you want to approve (network access to .+)\?$`)

	// codexShortcutHintRe matches the shortcut Codex appends to a menu label:
	// " (y)", " (esc)".
	codexShortcutHintRe = regexp.MustCompile(`\s+\([^()\s][^()]{0,15}\)$`)
)

// codexApprovalFooter starts the overlay's footer.
const codexApprovalFooter = "Press enter to confirm"

// codexApprovalTitleMaxRows bounds how many rows a wrapped title may span.
const codexApprovalTitleMaxRows = 3

// parseCodexApprovalOverlay parses a live Codex approval overlay at the bottom
// of the pane. It returns nil unless the pane ends with an approval title, an
// indented body, and a well-formed numbered menu (and the optional footer).
func parseCodexApprovalOverlay(lines []string) *parsedApproval {
	end := trimBlankTail(lines, len(lines))
	for i := end - 1; i >= 0 && i >= end-2; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), codexApprovalFooter) {
			end = trimBlankTail(lines, i)
			break
		}
	}

	// The menu is the last run of rows that starts at option 1.
	menu := -1
	for i := end - 1; i >= 0; i-- {
		if m := approvalOptionRe.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil && m[2] == "1" {
			menu = i
			break
		}
	}
	if menu < 0 {
		return nil
	}
	options, ok := parseApprovalMenu(lines[menu:end])
	if !ok {
		return nil
	}
	for i := range options {
		options[i].Label = codexShortcutHintRe.ReplaceAllString(options[i].Label, "")
	}

	// The title is the nearest one above the menu, with only overlay rows
	// (blank or indented) between them.
	for i := menu - 1; i >= 0; i-- {
		row := lines[i]
		if strings.TrimSpace(row) == "" {
			continue
		}
		if leadingSpaces(row) < 2 {
			return nil
		}
		question, next, ok := codexApprovalTitleAt(lines[:menu], i)
		if !ok {
			continue
		}
		approval := &parsedApproval{Question: question, Options: options}
		describeCodexApproval(approval, lines[next:menu])
		return approval
	}
	return nil
}

// codexApprovalTitleAt reads an approval title that starts at lines[i],
// rejoining the rows a narrow pane wrapped it onto. It returns the title, the
// index of the row after it, and whether lines[i] starts one.
func codexApprovalTitleAt(lines []string, i int) (string, int, bool) {
	text := strings.TrimSpace(lines[i])
	if !strings.HasPrefix(text, "Would you like to ") && !strings.HasPrefix(text, "Do you want to approve network access") {
		return "", 0, false
	}
	next := i + 1
	for ; next < len(lines) && next-i < codexApprovalTitleMaxRows && !codexApprovalQuestionRe.MatchString(text); next++ {
		row := strings.TrimSpace(lines[next])
		if row == "" {
			break
		}
		text += " " + row
	}
	if !codexApprovalQuestionRe.MatchString(text) {
		return "", 0, false
	}
	return text, next, true
}

// describeCodexApproval fills in the tool name and subject from the title and
// the body rows between it and the menu. Body fields are separated by blank
// rows: "Reason: …" (wrapped), "$ command" (one row per command line),
// "Description: …", "Destination: …" and "Permission rule: …".
func describeCodexApproval(approval *parsedApproval, body []string) {
	fields := codexBodyFields(body)
	switch {
	case strings.HasPrefix(approval.Question, "Would you like to make the following edits"):
		approval.ToolName = "apply_patch"
		approval.Input = fields["Destination: "]
		approval.Description = fields["Description: "]
	case strings.HasPrefix(approval.Question, "Would you like to grant these permissions"):
		approval.ToolName = "request_permissions"
		approval.Input = fields["Permission rule: "]
		approval.Description = fields["Reason: "]
	default:
		approval.ToolName = "exec"
		approval.Command = fields["$ "]
		approval.Input = approval.Command
		approval.Description = fields["Reason: "]
		if m := codexNetworkHostRe.FindStringSubmatch(approval.Question); m != nil {
			approval.Input = m[1]
		}
	}
	if approval.Input == "" {
		approval.Input = approval.Question
	}
	approval.Input = truncateApprovalInput(approval.Input)
}

// codexBodyFields splits the overlay body into its blank-separated fields,
// keyed by their label. Prose fields rejoin their wrapped rows with spaces; the
// command keeps one line per row.
func codexBodyFields(body []string) map[string]string {
	fields := make(map[string]string)
	var key string
	var rows []string
	flush := func() {
		if key == "" {
			return
		}
		if key == "$ " {
			fields[key] = strings.Join(rows, "\n")
		} else {
			fields[key] = strings.Join(rows, " ")
		}
		key, rows = "", nil
	}
	for _, line := range body {
		row := strings.TrimSpace(line)
		if row == "" {
			flush()
			continue
		}
		if key != "$ " {
			// A labeled row starts a field; prose fields can be adjacent
			// ("Description: …" directly above "Destination: …").
			for _, label := range []string{"$ ", "Reason: ", "Description: ", "Destination: ", "Permission rule: "} {
				if strings.HasPrefix(row, label) {
					flush()
					key, row = label, strings.TrimPrefix(row, label)
					break
				}
			}
			if key == "" {
				continue
			}
		} else {
			// Keep the command's own indentation; only the overlay's goes.
			row = strings.TrimPrefix(strings.TrimRight(line, " "), "  ")
		}
		rows = append(rows, row)
	}
	flush()
	return fields
}

// trimBlankTail returns end moved up past the blank rows above it.
func trimBlankTail(lines []string, end int) int {
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return end
}
