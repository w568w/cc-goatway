package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func rewriteMessagesBody(body []byte, config Config, sessionID string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		// Non-JSON body, return as-is
		return body, nil
	}

	delete(root, "tool_choice") // send by OpenCode, but not by Claude Code. Remove to avoid fingerprinting.

	root["thinking"] = map[string]any{"type": "adaptive"}
	root["output_config"] = map[string]any{"effort": "medium"}

	metadata, _ := root["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		root["metadata"] = metadata
	}

	userID := map[string]any{}
	if rawUserID, ok := metadata["user_id"].(string); ok && rawUserID != "" {
		_ = json.Unmarshal([]byte(rawUserID), &userID)
	}
	userID["device_id"] = config.Identity.DeviceID
	userID["account_uuid"] = config.Identity.AccountUUID
	userID["session_id"] = sessionID
	encodedUserID, err := json.Marshal(userID)
	if err != nil {
		return nil, err
	}
	metadata["user_id"] = string(encodedUserID)

	if isOpenCodeMessagesRequest(root) {
		root["system"] = rewriteOpenCodeSystem(root["system"])
		prependClaudeCodeReminder(root)
	}

	return json.Marshal(root)
}

func isOpenCodeMessagesRequest(root map[string]any) bool {
	if strings.Contains(strings.ToLower(extractSystemText(root["system"])), "opencode") {
		return true
	}

	tools, _ := root["tools"].([]any)
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		name, _ := tool["name"].(string)
		switch {
		case name == "question", name == "todowrite", strings.HasPrefix(name, "chrome-devtools_"):
			return true
		}
	}

	return false
}

func rewriteOpenCodeSystem(system any) []any {
	text := strings.TrimSpace(sanitizeOpenCodeSystemText(extractSystemText(system)))
	if text == "" {
		text = "You are an interactive agent that helps users with software engineering tasks."
	}

	return []any{
		claudeCodeSystemPrefix,
		map[string]any{
			"type": "text",
			"text": "\n" + text,
		},
	}
}

func extractSystemText(system any) string {
	switch value := system.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			switch block := block.(type) {
			case string:
				if block != "" {
					parts = append(parts, block)
				}
			case map[string]any:
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func sanitizeOpenCodeSystemText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "You are OpenCode, the best coding agent on the planet.\n\n", "")
	text = strings.ReplaceAll(text, "You are OpenCode, the best coding agent on the planet.", "")

	paragraphs := strings.Split(text, "\n\n")
	filtered := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		trimmed := strings.TrimSpace(paragraph)
		lower := strings.ToLower(trimmed)
		if trimmed == "" {
			continue
		}
		if strings.Contains(lower, "opencode.ai/docs") || strings.Contains(lower, "github.com/anomalyco/opencode") || strings.Contains(lower, "ctrl+p") {
			continue
		}
		if strings.Contains(lower, "when the user directly asks about opencode") || strings.Contains(lower, "to give feedback") {
			continue
		}
		if strings.Contains(lower, "todowrite tools") || strings.Contains(lower, "webfetch tool to gather information from opencode docs") {
			continue
		}
		filtered = append(filtered, strings.ReplaceAll(paragraph, "OpenCode", "Claude Code"))
	}

	return strings.TrimSpace(strings.Join(filtered, "\n\n"))
}

func prependClaudeCodeReminder(root map[string]any) {
	messages, _ := root["messages"].([]any)
	if len(messages) == 0 {
		return
	}

	first, _ := messages[0].(map[string]any)
	if first == nil || first["role"] != "user" {
		return
	}

	content := first["content"]
	switch value := content.(type) {
	case string:
		first["content"] = []any{
			map[string]any{"type": "text", "text": claudeCodeReminder},
			map[string]any{"type": "text", "text": value},
		}
	case []any:
		if len(value) > 0 {
			if block, _ := value[0].(map[string]any); block != nil {
				if text, _ := block["text"].(string); text == claudeCodeReminder {
					return
				}
			}
		}
		first["content"] = append([]any{map[string]any{"type": "text", "text": claudeCodeReminder}}, value...)
	}
}

func standardizeMessagesHeaders(headers http.Header, sessionID string) {
	headers.Set("Accept", "application/json")
	headers.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	headers.Set("User-Agent", "claude-cli/2.1.91 (external, cli)")
	headers.Set("X-App", "cli")
	headers.Set("X-Claude-Code-Session-Id", sessionID)

	// Stainless is an agent library, used by Claude Code.
	// Not sure if these are all necessary, but we'll include them for good measure.
	headers.Set("X-Stainless-Arch", "x64")
	headers.Set("X-Stainless-Lang", "js")
	headers.Set("X-Stainless-Os", "Linux")
	headers.Set("X-Stainless-Package-Version", "0.80.0")
	headers.Set("X-Stainless-Retry-Count", "0")
	headers.Set("X-Stainless-Runtime", "node")
	headers.Set("X-Stainless-Runtime-Version", "v24.3.0")
	headers.Set("X-Stainless-Timeout", "600")

	for _, beta := range claudeCodeBetaHeaders {
		ensureHeaderListValue(headers, "Anthropic-Beta", beta)
	}
}
