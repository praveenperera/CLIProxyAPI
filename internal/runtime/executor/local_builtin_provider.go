package executor

// LocalBuiltinProvider identifies the local built-in execution authority
func (e *ClaudeExecutor) LocalBuiltinProvider() string { return "claude" }

// LocalBuiltinProvider identifies the local built-in execution authority
func (e *CodexExecutor) LocalBuiltinProvider() string { return "codex" }

// LocalBuiltinProvider identifies the local built-in execution authority
func (e *CodexAutoExecutor) LocalBuiltinProvider() string { return "codex" }

// LocalBuiltinProvider identifies the local built-in execution authority
func (e *CodexWebsocketsExecutor) LocalBuiltinProvider() string { return "codex" }
