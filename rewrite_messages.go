package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const mcpToolPrefix = "mcp_"

var toolNameWithPrefixPattern = regexp.MustCompile(`("name"\s*:\s*")mcp_([^"]+)(")`)

func rewriteMessagesBody(body []byte, config Config, sessionID string, userAgent string) ([]byte, error) {
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

	if isOpenCodeMessagesRequest(root, userAgent) {
		root["system"] = rewriteOpenCodeSystem(root["system"])
		prefixOpenCodeToolNames(root)
		prependClaudeCodeReminder(root)
	}

	return json.Marshal(root)
}

func isOpenCodeMessagesRequestBody(body []byte, userAgent string) bool {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	return isOpenCodeMessagesRequest(root, userAgent)
}

func isOpenCodeMessagesRequest(root map[string]any, userAgent string) bool {
	if strings.Contains(strings.ToLower(userAgent), "opencode") {
		return true
	}
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
	original := strings.TrimSpace(extractSystemText(system))
	rewritten := original

	switch detectOpenCodeMode(original) {
	case "plan":
		rewritten = planModeSystemPrompt
	case "build":
		rewritten = buildModeSystemPrompt
	case "other":
		if rewritten == "" {
			rewritten = "You are an interactive agent that helps users with software engineering tasks."
		}
	}

	return []any{
		claudeCodeSystemPrefix,
		map[string]any{
			"type": "text",
			"text": "\n" + rewritten,
		},
	}
}

func detectOpenCodeMode(systemText string) string {
	lower := strings.ToLower(systemText)

	if strings.Contains(lower, "plan mode active") ||
		(strings.Contains(lower, "read-only") && strings.Contains(lower, "must not make any edits")) {
		return "plan"
	}

	if strings.Contains(lower, "your operational mode has changed from plan to build") ||
		strings.Contains(lower, "you are no longer in read-only mode") {
		return "build"
	}

	if strings.Contains(lower, "you are opencode, the best coding agent on the planet.") ||
		strings.Contains(lower, "interactive cli tool that helps users with software engineering tasks") {
		return "build"
	}

	return "other"
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

func prefixOpenCodeToolNames(root map[string]any) {
	tools, _ := root["tools"].([]any)
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if tool == nil {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		tool["name"] = prefixToolName(name)
	}

	messages, _ := root["messages"].([]any)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if message == nil {
			continue
		}
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			if block == nil {
				continue
			}
			if blockType, _ := block["type"].(string); blockType != "tool_use" {
				continue
			}
			name, _ := block["name"].(string)
			if name == "" {
				continue
			}
			block["name"] = prefixToolName(name)
		}
	}
}

func prefixToolName(name string) string {
	if strings.HasPrefix(name, mcpToolPrefix) {
		return mcpToolPrefix + uppercaseFirst(name[len(mcpToolPrefix):])
	}
	return mcpToolPrefix + uppercaseFirst(name)
}

func unprefixToolName(name string) string {
	if !strings.HasPrefix(name, mcpToolPrefix) {
		return name
	}
	return lowercaseFirst(name[len(mcpToolPrefix):])
}

func stripToolPrefixInJSONText(text string) string {
	return toolNameWithPrefixPattern.ReplaceAllStringFunc(text, func(match string) string {
		groups := toolNameWithPrefixPattern.FindStringSubmatch(match)
		if len(groups) != 4 {
			return match
		}
		return groups[1] + unprefixToolName(mcpToolPrefix+groups[2]) + groups[3]
	})
}

func uppercaseFirst(value string) string {
	if value == "" {
		return value
	}
	r, size := utf8.DecodeRuneInString(value)
	if r == utf8.RuneError && size == 0 {
		return value
	}
	return string(unicode.ToUpper(r)) + value[size:]
}

func lowercaseFirst(value string) string {
	if value == "" {
		return value
	}
	r, size := utf8.DecodeRuneInString(value)
	if r == utf8.RuneError && size == 0 {
		return value
	}
	return string(unicode.ToLower(r)) + value[size:]
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
