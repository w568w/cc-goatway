package main

import (
	"encoding/json"
	"net/http"
	"regexp"
)

var (
	billingHeaderLine     = regexp.MustCompile(`x-anthropic-billing-header:[^\n]+\n?`)
	billingHeaderText     = regexp.MustCompile(`^\s*x-anthropic-billing-header:`)
	claudeCodeBetaHeaders = []string{
		"claude-code-20250219",
		oauthBetaHeader,
		"context-1m-2025-08-07",
		"redact-thinking-2026-02-12",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"effort-2025-11-24",
	}
)

func rewriteMessagesBody(body []byte, config Config, sessionID string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		// Non-JSON body, return as-is
		return body, nil
	}

	delete(root, "tool_choice") // send by OpenCode, but not by Claude Code. Remove to avoid fingerprinting.

	// TODO: Are they fine to omit?
	// root["thinking"] = map[string]any{"type": "adaptive"}
	// root["output_config"] = map[string]any{"effort": "medium"}

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

	if system, ok := root["system"]; ok {
		root["system"] = rewriteMessagesSystem(system)
	}

	return json.Marshal(root)
}

func rewriteMessagesSystem(system any) any {
	switch value := system.(type) {
	case string:
		return billingHeaderLine.ReplaceAllString(value, "")
	case []any:
		compacted := make([]any, 0, len(value))
		for _, block := range value {
			switch block := block.(type) {
			case string:
				if billingHeaderText.MatchString(block) {
					continue
				}
				compacted = append(compacted, block)
			case map[string]any:
				text, _ := block["text"].(string)
				if billingHeaderText.MatchString(text) {
					continue
				}
				compacted = append(compacted, block)
			default:
				compacted = append(compacted, block)
			}
		}
		return compacted
	default:
		return system
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
