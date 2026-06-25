package llm

// EstimateTokens is the fallback for providers that do not emit a
// usage block (Ollama in OpenAI mode is the common one). The rule
// of thumb is "1 token per 4 characters of English text" — for
// Russian the real ratio is closer to 1 token per 2.5 characters,
// so this number is conservative and slightly under-reports for
// Cyrillic. Operators who need exact numbers should switch to
// "usage" mode and a provider that returns the block.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	return (len(text) + 3) / 4
}

// EstimateMessages returns a rough prompt-side token count. The
// tool definitions are accounted for in tool count; the message
// bodies are estimated as plain text. This is good enough for a
// per-session "we used 12k tok" log line and bad enough that
// nobody should use it for billing.
func EstimateMessages(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m.Content)
		if m.Name != "" {
			total += EstimateTokens(m.Name)
		}

		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Function.Name) + EstimateTokens(tc.Function.Arguments)
		}
	}

	return total
}
