// Package proxy 上下文截断保护
// 防止超长历史记录导致新账号被秒封
package proxy

import (
	"fmt"
	"strings"
)

const (
	// 安全的 Token 阈值（保守估计）
	SafeTokenThreshold = 3500
	// 每个 Token 约 4 个字符（英文）
	CharsPerToken = 4
	// 最小保留轮数
	MinRounds = 2
	// 最大保留轮数
	MaxRounds = 5
)

// TruncateClaudeMessages 截断 Claude 消息历史
// isNewSession: 是否是新账号接手老对话
// messages: 原始消息数组
// 返回：截断后的消息数组
func TruncateClaudeMessages(messages []ClaudeMessage, isNewSession bool) []ClaudeMessage {
	if !isNewSession || len(messages) <= MinRounds*2 {
		// 不是新会话或消息数量较少，不截断
		return messages
	}

	// 估算总 Token 数
	totalTokens := estimateClaudeMessagesTokens(messages)
	
	if totalTokens <= SafeTokenThreshold {
		// 在安全阈值内，不截断
		fmt.Printf("[ContextTruncation] Total tokens: %d (safe), no truncation needed\n", totalTokens)
		return messages
	}

	// 需要截断：保留最近的几轮对话
	truncated := truncateToRecentRounds(messages, MaxRounds)
	newTokens := estimateClaudeMessagesTokens(truncated)
	
	fmt.Printf("[ContextTruncation] ⚠️  NEW SESSION detected with long history\n")
	fmt.Printf("[ContextTruncation] Original: %d messages, ~%d tokens\n", len(messages), totalTokens)
	fmt.Printf("[ContextTruncation] Truncated: %d messages, ~%d tokens (kept last %d rounds)\n", 
		len(truncated), newTokens, MaxRounds)
	
	return truncated
}

// TruncateOpenAIMessages 截断 OpenAI 消息历史
func TruncateOpenAIMessages(messages []OpenAIMessage, isNewSession bool) []OpenAIMessage {
	if !isNewSession || len(messages) <= MinRounds*2 {
		return messages
	}

	totalTokens := estimateOpenAIMessagesTokens(messages)
	
	if totalTokens <= SafeTokenThreshold {
		fmt.Printf("[ContextTruncation] Total tokens: %d (safe), no truncation needed\n", totalTokens)
		return messages
	}

	// 保留 system 消息 + 最近的几轮对话
	var systemMessages []OpenAIMessage
	var nonSystemMessages []OpenAIMessage
	
	for _, msg := range messages {
		if msg.Role == "system" {
			systemMessages = append(systemMessages, msg)
		} else {
			nonSystemMessages = append(nonSystemMessages, msg)
		}
	}
	
	truncated := truncateOpenAIToRecentRounds(nonSystemMessages, MaxRounds)
	result := append(systemMessages, truncated...)
	newTokens := estimateOpenAIMessagesTokens(result)
	
	fmt.Printf("[ContextTruncation] ⚠️  NEW SESSION detected with long history\n")
	fmt.Printf("[ContextTruncation] Original: %d messages, ~%d tokens\n", len(messages), totalTokens)
	fmt.Printf("[ContextTruncation] Truncated: %d messages, ~%d tokens (kept last %d rounds)\n", 
		len(result), newTokens, MaxRounds)
	
	return result
}

// truncateToRecentRounds 保留最近的 N 轮对话（user-assistant 对）
func truncateToRecentRounds(messages []ClaudeMessage, maxRounds int) []ClaudeMessage {
	if len(messages) == 0 {
		return messages
	}

	// 从后往前找 user-assistant 对
	var rounds [][]ClaudeMessage
	var currentRound []ClaudeMessage
	
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		currentRound = append([]ClaudeMessage{msg}, currentRound...)
		
		// 遇到 user 消息，表示一轮对话的开始
		if msg.Role == "user" {
			rounds = append([][]ClaudeMessage{currentRound}, rounds...)
			currentRound = nil
			
			if len(rounds) >= maxRounds {
				break
			}
		}
	}
	
	// 展平
	var result []ClaudeMessage
	for _, round := range rounds {
		result = append(result, round...)
	}
	
	return result
}

// truncateOpenAIToRecentRounds 保留最近的 N 轮对话
func truncateOpenAIToRecentRounds(messages []OpenAIMessage, maxRounds int) []OpenAIMessage {
	if len(messages) == 0 {
		return messages
	}

	var rounds [][]OpenAIMessage
	var currentRound []OpenAIMessage
	
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		currentRound = append([]OpenAIMessage{msg}, currentRound...)
		
		if msg.Role == "user" {
			rounds = append([][]OpenAIMessage{currentRound}, rounds...)
			currentRound = nil
			
			if len(rounds) >= maxRounds {
				break
			}
		}
	}
	
	var result []OpenAIMessage
	for _, round := range rounds {
		result = append(result, round...)
	}
	
	return result
}

// estimateClaudeMessagesTokens 估算 Claude 消息的 Token 数
func estimateClaudeMessagesTokens(messages []ClaudeMessage) int {
	totalChars := 0
	
	for _, msg := range messages {
		// 估算内容长度
		if s, ok := msg.Content.(string); ok {
			totalChars += len(s)
		} else if blocks, ok := msg.Content.([]interface{}); ok {
			for _, b := range blocks {
				if block, ok := b.(map[string]interface{}); ok {
					if text, ok := block["text"].(string); ok {
						totalChars += len(text)
					}
				}
			}
		}
	}
	
	// 字符数 / 4 ≈ Token 数（保守估计）
	return totalChars / CharsPerToken
}

// estimateOpenAIMessagesTokens 估算 OpenAI 消息的 Token 数
func estimateOpenAIMessagesTokens(messages []OpenAIMessage) int {
	totalChars := 0
	
	for _, msg := range messages {
		if s, ok := msg.Content.(string); ok {
			totalChars += len(s)
		} else if parts, ok := msg.Content.([]interface{}); ok {
			for _, p := range parts {
				if part, ok := p.(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok {
						totalChars += len(text)
					}
				}
			}
		}
		
		// 工具调用也计入
		for _, tc := range msg.ToolCalls {
			totalChars += len(tc.Function.Name)
			totalChars += len(tc.Function.Arguments)
		}
	}
	
	return totalChars / CharsPerToken
}

// IsNewSessionForAccount 检测是否是新账号接手老对话
// 通过检查账号是否有绑定的 AWS ConversationID 来判断
func IsNewSessionForAccount(accountID string, poolGetter func(string) string) bool {
	awsConvID := poolGetter(accountID)
	isNew := awsConvID == ""
	
	if isNew {
		fmt.Printf("[ContextTruncation] 🆕 NEW SESSION: Account %s has no AWS ConversationID\n", accountID)
	} else {
		fmt.Printf("[ContextTruncation] Existing session: Account %s has AWS ConversationID %s\n", 
			accountID, awsConvID)
	}
	
	return isNew
}

// AddTruncationNotice 在截断后的消息中添加提示
func AddTruncationNotice(messages []ClaudeMessage) []ClaudeMessage {
	if len(messages) == 0 {
		return messages
	}

	// 在第一条 user 消息前添加提示
	notice := ClaudeMessage{
		Role:    "user",
		Content: "[Context truncated: showing last " + fmt.Sprintf("%d", MaxRounds) + " rounds to prevent token overflow]",
	}
	
	return append([]ClaudeMessage{notice}, messages...)
}

// ShouldTruncate 判断是否需要截断
func ShouldTruncate(messageCount int, estimatedTokens int, isNewSession bool) bool {
	if !isNewSession {
		return false
	}
	
	// 消息数量超过阈值或 Token 数超过阈值
	return messageCount > MinRounds*2 || estimatedTokens > SafeTokenThreshold
}

// GetTruncationStats 获取截断统计信息
func GetTruncationStats(original, truncated []ClaudeMessage) string {
	originalTokens := estimateClaudeMessagesTokens(original)
	truncatedTokens := estimateClaudeMessagesTokens(truncated)
	
	return fmt.Sprintf("Truncated %d→%d messages, ~%d→~%d tokens (saved ~%d tokens)",
		len(original), len(truncated),
		originalTokens, truncatedTokens,
		originalTokens-truncatedTokens)
}
