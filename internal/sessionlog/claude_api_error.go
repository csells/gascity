package sessionlog

import "strings"

// normalizeClaudeAPIError reclassifies an assistant entry Claude Code flagged
// isApiErrorMessage as a provider-error system event, so a failed request
// does not read as a reply. Only the CLI's explicit flag counts: a refusal or
// a reply quoting "API Error" stays an assistant message. The message and Raw
// are kept, so the error text and the native record remain available.
func normalizeClaudeAPIError(entry *Entry) {
	if entry.Type != "assistant" || !entry.IsAPIErrorMessage {
		return
	}
	text := entry.TextContent()
	if text == "" {
		var parts []string
		for _, block := range entry.ContentBlocks() {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "Claude reported an API error"
	}
	entry.Type = "system"
	entry.SystemEvent = &SystemEvent{
		Kind:     "error",
		Category: "provider_error",
		Code:     jsonStringValue(entry.APIError),
		Message:  text,
	}
}
