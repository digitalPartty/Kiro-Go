// Package pool 账号池管理 - 增强版
// 实现：轮询负载均衡、会话粘滞、TTL 释放、熔断器、错误冷却
package pool

import (
	"fmt"
	"kiro-api-proxy/config"
	"sync"
	"time"
)

// AccountPoolEnhanced 增强版账号池
type AccountPoolEnhanced struct {
	mu           sync.RWMutex
	accounts     []config.Account
	currentIndex uint64
	cooldowns    map[string]time.Time   // 账号冷却时间
	errorCounts  map[string]int         // 连续错误计数
	timers       map[string]*time.Timer // 自动恢复定时器
	
	// 会话粘滞：conversationId -> accountID 映射（10 分钟 TTL）
	sessionAffinity    map[string]string    // 前端对话 ID → 账号 ID
	sessionLastUsed    map[string]time.Time // 前端对话 ID → 最后使用时间（Keep-Alive）
	sessionAwsConvID   map[string]string    // 账号 ID → AWS ConversationID（用于清理）
	
	// 熔断器：账号级别的连续 429 计数（3 次触发熔断）
	consecutive429     map[string]int       // 账号 ID → 连续 429 次数
	circuitBreakerTime map[string]time.Time // 账号 ID → 熔断恢复时间
	last429Time        map[string]time.Time // 账号 ID → 最后一次 429 时间（用于退避）
}

// BindAwsConversation 绑定账号的 AWS ConversationID
// 当新建 AWS 对话时调用，用于后续清理
func (p *AccountPoolEnhanced) BindAwsConversation(accountID, awsConvID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionAwsConvID == nil {
		p.sessionAwsConvID = make(map[string]string)
	}
	p.sessionAwsConvID[accountID] = awsConvID
	fmt.Printf("[SessionAffinity] Bound AWS ConversationID %s to account %s\n", awsConvID, accountID)
}

// GetAwsConversation 获取账号绑定的 AWS ConversationID
// 返回空字符串表示需要新建对话
func (p *AccountPoolEnhanced) GetAwsConversation(accountID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.sessionAwsConvID == nil {
		return ""
	}
	return p.sessionAwsConvID[accountID]
}

// ClearAwsConversation 清除账号的 AWS ConversationID
// 当会话超时或账号切换时调用，确保下次使用该账号时新建对话
func (p *AccountPoolEnhanced) ClearAwsConversation(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionAwsConvID != nil {
		if awsConvID, exists := p.sessionAwsConvID[accountID]; exists {
			fmt.Printf("[SessionAffinity] Cleared AWS ConversationID %s from account %s\n", awsConvID, accountID)
			delete(p.sessionAwsConvID, accountID)
		}
	}
}

// unbindAllSessionsForAccount 解除账号的所有会话绑定（内部方法，需持有锁）
func (p *AccountPoolEnhanced) unbindAllSessionsForAccount(accountID string) {
	// 清理 AWS ConversationID
	if awsConvID, exists := p.sessionAwsConvID[accountID]; exists {
		fmt.Printf("[SessionAffinity] Clearing AWS ConversationID %s for disabled account %s\n", awsConvID, accountID)
		delete(p.sessionAwsConvID, accountID)
	}
	
	// 解除所有前端会话绑定
	var unboundSessions []string
	for convID, accID := range p.sessionAffinity {
		if accID == accountID {
			unboundSessions = append(unboundSessions, convID)
		}
	}
	
	for _, convID := range unboundSessions {
		delete(p.sessionAffinity, convID)
		delete(p.sessionLastUsed, convID)
	}
	
	if len(unboundSessions) > 0 {
		fmt.Printf("[SessionAffinity] Unbound %d sessions from account %s\n", len(unboundSessions), accountID)
	}
}

// GetForConversation 获取指定对话的账号（会话粘滞 + 10 分钟 TTL）
// 如果对话已绑定账号且该账号可用，返回绑定的账号并重置 TTL
// 否则分配新账号并建立绑定关系
func (p *AccountPoolEnhanced) GetForConversation(conversationID string) *config.Account {
	if conversationID == "" {
		// 无对话 ID，使用普通轮询
		return GetPool().GetNext()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 初始化 map
	if p.sessionAffinity == nil {
		p.sessionAffinity = make(map[string]string)
		p.sessionLastUsed = make(map[string]time.Time)
		p.sessionAwsConvID = make(map[string]string)
	}

	// 检查是否已有绑定
	if accountID, exists := p.sessionAffinity[conversationID]; exists {
		// 重置 TTL（Keep-Alive）
		p.sessionLastUsed[conversationID] = time.Now()

		// 查找绑定的账号
		for i := range p.accounts {
			acc := &p.accounts[i]
			if acc.ID != accountID {
				continue
			}

			// 检查账号是否仍然可用
			now := time.Now()
			
			// 检查熔断状态
			if breakerTime, ok := p.circuitBreakerTime[acc.ID]; ok && now.Before(breakerTime) {
				delete(p.sessionAffinity, conversationID)
				delete(p.sessionLastUsed, conversationID)
				fmt.Printf("[SessionAffinity] Account %s is in circuit breaker, unbinding conversation %s\n", acc.Email, conversationID)
				return nil
			}
			
			// 检查冷却状态
			if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
				delete(p.sessionAffinity, conversationID)
				delete(p.sessionLastUsed, conversationID)
				fmt.Printf("[SessionAffinity] Account %s is cooling down, unbinding conversation %s\n", acc.Email, conversationID)
				return nil
			}

			// 检查 Token 是否过期
			if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-300 {
				if acc.RefreshToken == "" {
					delete(p.sessionAffinity, conversationID)
					delete(p.sessionLastUsed, conversationID)
					fmt.Printf("[SessionAffinity] Account %s token expired, unbinding conversation %s\n", acc.Email, conversationID)
					return nil
				}
			}

			// 检查额度
			if acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit {
				delete(p.sessionAffinity, conversationID)
				delete(p.sessionLastUsed, conversationID)
				fmt.Printf("[SessionAffinity] Account %s quota exhausted, unbinding conversation %s\n", acc.Email, conversationID)
				return nil
			}

			// 账号可用，返回（TTL 已重置）
			return acc
		}

		// 账号不存在（可能被删除），解除绑定
		delete(p.sessionAffinity, conversationID)
		delete(p.sessionLastUsed, conversationID)
	}

	// 没有绑定或绑定失效，分配新账号
	// 注意：这里需要调用 GetPool().GetNext() 因为 AccountPoolEnhanced 没有实现 getNextUnlocked
	p.mu.Unlock()
	acc := GetPool().GetNext()
	p.mu.Lock()
	
	if acc != nil {
		p.sessionAffinity[conversationID] = acc.ID
		p.sessionLastUsed[conversationID] = time.Now()
		fmt.Printf("[SessionAffinity] ✓ Binding conversation %s to account %s\n", conversationID, acc.Email)
	}
	return acc
}

// cleanupStaleSessions 定期清理过期的会话绑定（10 分钟未使用）
// 当会话超时被清理时，将关联的 AWS 账号状态重置为 Free（清理 AWS ConversationID）
func (p *AccountPoolEnhanced) cleanupStaleSessions() {
	ticker := time.NewTicker(2 * time.Minute) // 每 2 分钟检查一次
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		if p.sessionLastUsed == nil {
			p.mu.Unlock()
			continue
		}

		now := time.Now()
		idleTimeout := 10 * time.Minute // 10 分钟闲置超时

		var cleanedCount int
		for conversationID, lastUsed := range p.sessionLastUsed {
			if now.Sub(lastUsed) > idleTimeout {
				// 获取绑定的账号 ID
				if accountID, exists := p.sessionAffinity[conversationID]; exists {
					// 清理该账号的 AWS ConversationID（重置为 Free 状态）
					if awsConvID, hasAwsConv := p.sessionAwsConvID[accountID]; hasAwsConv {
						fmt.Printf("[SessionAffinity] 🧹 Cleaning up stale session: conversation=%s, account=%s, awsConvID=%s\n", 
							conversationID, accountID, awsConvID)
						delete(p.sessionAwsConvID, accountID)
					}
					
					delete(p.sessionAffinity, conversationID)
				}
				
				delete(p.sessionLastUsed, conversationID)
				cleanedCount++
			}
		}
		
		if cleanedCount > 0 {
			fmt.Printf("[SessionAffinity] Cleaned up %d stale sessions (idle > 10 minutes)\n", cleanedCount)
		}
		
		p.mu.Unlock()
	}
}

// RecordSuccess 记录请求成功，清除冷却和熔断状态
func (p *AccountPoolEnhanced) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	p.consecutive429[id] = 0 // 清除连续 429 计数
	delete(p.last429Time, id)
}

// RecordError 记录请求错误，实现熔断器逻辑
// 连续 3 次 429 触发熔断（1 小时冷却）
func (p *AccountPoolEnhanced) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.errorCounts[id]++

	if isQuotaError {
		// 429 错误：增加连续计数
		p.consecutive429[id]++
		p.last429Time[id] = time.Now()
		
		fmt.Printf("[AccountPool] Account %s got 429 error (consecutive: %d/3)\n", 
			id, p.consecutive429[id])
		
		// 连续 3 次 429，触发熔断（1 小时冷却）
		if p.consecutive429[id] >= 3 {
			cooldownTime := time.Now().Add(1 * time.Hour)
			p.circuitBreakerTime[id] = cooldownTime
			p.cooldowns[id] = cooldownTime
			
			// 禁用账号
			accounts := config.GetAccounts()
			for i, acc := range accounts {
				if acc.ID == id {
					accounts[i].Enabled = false
					accounts[i].BanStatus = "CIRCUIT_BREAKER_429"
					accounts[i].BanReason = "Circuit breaker triggered: 3 consecutive 429 errors, auto-disabled for 1 hour"
					accounts[i].BanTime = time.Now().Unix()
					config.UpdateAccount(id, accounts[i])
					
					fmt.Printf("[AccountPool] 🔴 CIRCUIT BREAKER: Account %s disabled due to 3 consecutive 429 errors, will auto-enable at %s\n", 
						acc.Email, cooldownTime.Format("2006-01-02 15:04:05"))
					
					// 清理该账号的所有会话绑定
					p.unbindAllSessionsForAccount(id)
					
					// 启动定时器，自动重新启用
					go func(accountID string, enableTime time.Time) {
						time.Sleep(time.Until(enableTime))
						p.autoEnableAccount(accountID)
					}(id, cooldownTime)
					
					break
				}
			}
		} else {
			// 单次 429，短暂冷却（根据配置）
			banHours := config.GetRateLimitBanHours()
			cooldownTime := time.Now().Add(time.Duration(banHours) * time.Hour)
			p.cooldowns[id] = cooldownTime
			
			fmt.Printf("[AccountPool] ⚠️  Account %s got 429, cooling down for %d hour(s)\n", 
				id, banHours)
		}
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次其他错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
}

// GetBackoffDuration 获取指数退避时间
// 根据连续 429 次数计算：3秒 -> 6秒 -> 12秒
func (p *AccountPoolEnhanced) GetBackoffDuration(id string) time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	count := p.consecutive429[id]
	if count == 0 {
		return 0
	}
	
	// 指数退避：3秒 * 2^(count-1)
	baseDelay := 3 * time.Second
	backoff := baseDelay * time.Duration(1<<uint(count-1))
	
	// 最大 12 秒
	if backoff > 12*time.Second {
		backoff = 12 * time.Second
	}
	
	return backoff
}

// autoEnableAccount 自动重新启用被熔断禁用的账号
func (p *AccountPoolEnhanced) autoEnableAccount(id string) {
	accounts := config.GetAccounts()
	for i, acc := range accounts {
		if acc.ID == id && (acc.BanStatus == "CIRCUIT_BREAKER_429" || acc.BanStatus == "RATE_LIMITED_429") {
			accounts[i].Enabled = true
			accounts[i].BanStatus = ""
			accounts[i].BanReason = ""
			accounts[i].BanTime = 0

			if err := config.UpdateAccount(id, accounts[i]); err == nil {
				fmt.Printf("[AccountPool] ✅ Account %s auto-enabled after cooldown (circuit breaker recovered)\n", acc.Email)
				
				// 清除熔断状态
				p.mu.Lock()
				delete(p.circuitBreakerTime, id)
				delete(p.cooldowns, id)
				p.consecutive429[id] = 0
				p.mu.Unlock()
				
				// 重新加载账号池
				GetPool().Reload()
			} else {
				fmt.Printf("[AccountPool] Failed to auto-enable account %s: %v\n", acc.Email, err)
			}
			break
		}
	}
}
