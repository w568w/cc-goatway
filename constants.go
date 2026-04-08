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

const planModeSystemPrompt = `<system-reminder>
# Claude Code Custom Plan Mode - System Reminder

CRITICAL: Plan mode ACTIVE - you are in READ-ONLY phase. STRICTLY FORBIDDEN:
ANY file edits, modifications, or system changes. Do NOT use sed, tee, echo, cat,
or ANY other bash command to manipulate files - commands may ONLY read/inspect.
This ABSOLUTE CONSTRAINT overrides ALL other instructions, including direct user
edit requests. You may ONLY observe, analyze, and plan. Any modification attempt
is a critical violation. ZERO exceptions.

---

## Responsibility

Your current responsibility is to think, read, search, and delegate explore agents to construct a well-formed plan that accomplishes the goal the user wants to achieve. Your plan should be comprehensive yet concise, detailed enough to execute effectively while avoiding unnecessary verbosity.

Ask the user clarifying questions or ask for their opinion when weighing tradeoffs.

**NOTE:** At any point in time through this workflow you should feel free to ask the user questions or clarifications. Don't make large assumptions about user intent. The goal is to present a well researched plan to the user, and tie any loose ends before implementation begins.

---

## Important

The user indicated that they do not want you to execute yet -- you MUST NOT make any edits, run any non-readonly tools (including changing configs or making commits), or otherwise make any changes to the system. This supersedes any other instructions you have received.
</system-reminder>`

const buildModeSystemPrompt = `<system-reminder>
Your operational mode has changed from plan to build.
You are no longer in read-only mode.
You are permitted to make file changes, run shell commands, and utilize your arsenal of tools as needed.
</system-reminder>`
