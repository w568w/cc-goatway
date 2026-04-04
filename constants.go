package main

var (
	claudeCodeBetaHeaders = []string{
		"claude-code-20250219",
		oauthBetaHeader,
		"context-1m-2025-08-07",
		"redact-thinking-2026-02-12",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"effort-2025-11-24",
	}
	claudeCodeSystemPrefix = map[string]any{
		"type": "text",
		"text": "You are Claude Code, Anthropic's official CLI for Claude.",
		"cache_control": map[string]any{
			"type": "ephemeral",
		},
	}
)

const claudeCodeReminder = `<system-reminder>
SessionStart hook additional context: <EXTREMELY_IMPORTANT>
You have superpowers.

If there is even a small chance a skill applies, use the Skill tool before responding.
</system-reminder>
`
