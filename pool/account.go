// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"fmt"
	"kiro-api-proxy/auth"
	"kiro-api-proxy/config"
	"sync"
	"sync/atomic"
	"time"
)

// AccountPool 账号池
type AccountPool struct {
	mu           sync.RWMutex
	accounts     []config.Account
	currentIndex uint64
	cooldowns    map[string]time.Time // 账号冷却时间
	errorCounts  map[string]int       // 连续错误计数
	timers       map[string]*time.Timer // 自动恢复定时器
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:   make(map[string]time.Time),
			errorCounts: make(map[string]int),
			timers:      make(map[string]*time.Timer),
		}
		pool.Reload()
		pool.restorePendingRecoveries()
	})
	return pool
}

// Reload 从配置重新加载账号
// 构建加权列表：weight<=1 出现 1 次，weight>=2 出现 weight 次
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	enabled := config.GetEnabledAccounts()
	var weighted []config.Account
	for _, a := range enabled {
		w := a.Weight
		if w < 1 {
			w = 1
		}
		for j := 0; j < w; j++ {
			weighted = append(weighted, a)
		}
	}
	p.accounts = weighted
}

// GetNext 获取下一个可用账号（加权轮询）
func (p *AccountPool) GetNext() *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)
	var expiredAccounts []*config.Account

	// 加权轮询查找可用账号
	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if seen[acc.ID] {
			continue
		}

		// 跳过冷却中的账号
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			continue
		}

		// 检查 Token 是否即将过期
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-300 {
			// 收集过期账号，稍后尝试刷新
			if acc.RefreshToken != "" {
				expiredAccounts = append(expiredAccounts, acc)
			}
			seen[acc.ID] = true
			continue
		}

		// 跳过额度已用尽的账号（适用于所有订阅类型）
		if acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit {
			seen[acc.ID] = true
			continue
		}

		return acc
	}

	// 无可用账号，尝试刷新过期的 Token
	if len(expiredAccounts) > 0 {
		fmt.Printf("[AccountPool] No available accounts, attempting to refresh %d expired tokens\n", len(expiredAccounts))
		for _, acc := range expiredAccounts {
			if refreshedAcc := p.tryRefreshToken(acc); refreshedAcc != nil {
				fmt.Printf("[AccountPool] ✅ Successfully refreshed token for account %s\n", acc.Email)
				return refreshedAcc
			}
		}
	}

	// 仍然无可用账号，返回冷却时间最短的（排除额度用尽的）
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		// 额度用尽的账号不作为 fallback
		if acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.errorCounts[id]++

	if isQuotaError {
		// 配额错误（429），立即禁用账号 6 小时
		cooldownTime := time.Now().Add(6 * time.Hour)
		p.cooldowns[id] = cooldownTime

		// 禁用账号
		accounts := config.GetAccounts()
		for i, acc := range accounts {
			if acc.ID == id {
				accounts[i].Enabled = false
				accounts[i].BanStatus = "RATE_LIMITED_429"
				accounts[i].BanReason = "Quota exhausted (429), auto-disabled for 6 hours"
				accounts[i].BanTime = time.Now().Unix()
				config.UpdateAccount(id, accounts[i])

				fmt.Printf("[AccountPool] ⚠️  Account %s disabled due to 429 error, will auto-enable at %s (6 hours)\n",
					acc.Email, cooldownTime.Format("2006-01-02 15:04:05"))

				// 启动定时器，6 小时后自动重新启用
				p.scheduleAutoEnable(id, cooldownTime)

				break
			}
		}
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次其他错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
}

// RecordAccountBanned 记录账号被封禁（403）
func (p *AccountPool) RecordAccountBanned(id string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 永久禁用账号（不自动恢复）
	accounts := config.GetAccounts()
	for i, acc := range accounts {
		if acc.ID == id {
			accounts[i].Enabled = false
			accounts[i].BanStatus = "ACCOUNT_BANNED_403"
			accounts[i].BanReason = fmt.Sprintf("Account banned or suspended: %s", reason)
			accounts[i].BanTime = time.Now().Unix()
			config.UpdateAccount(id, accounts[i])
			
			fmt.Printf("[AccountPool] ⛔ Account %s BANNED (403/SUSPENDED), manual intervention required\n", acc.Email)
			fmt.Printf("[AccountPool] Ban reason: %s\n", reason)
			
			break
		}
	}
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
			break
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	for _, acc := range p.accounts {
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].RequestCount++
			p.accounts[i].TotalTokens += tokens
			p.accounts[i].TotalCredits += credits
			p.accounts[i].LastUsed = time.Now().Unix()
			go config.UpdateAccountStats(id, p.accounts[i].RequestCount, p.accounts[i].ErrorCount, p.accounts[i].TotalTokens, p.accounts[i].TotalCredits, p.accounts[i].LastUsed)
			break
		}
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

// scheduleAutoEnable 调度账号自动恢复定时器
func (p *AccountPool) scheduleAutoEnable(id string, enableTime time.Time) {
	// 取消已存在的定时器
	if timer, exists := p.timers[id]; exists {
		timer.Stop()
		delete(p.timers, id)
	}

	duration := time.Until(enableTime)
	if duration <= 0 {
		// 已经到时间了，立即执行
		p.autoEnableAccount(id)
		return
	}

	// 创建新定时器
	timer := time.AfterFunc(duration, func() {
		p.mu.Lock()
		delete(p.timers, id)
		p.mu.Unlock()
		p.autoEnableAccount(id)
	})

	p.timers[id] = timer
}

// restorePendingRecoveries 启动时恢复所有待恢复的账号定时器
func (p *AccountPool) restorePendingRecoveries() {
	accounts := config.GetAccounts()
	now := time.Now()

	for _, acc := range accounts {
		if acc.BanStatus == "RATE_LIMITED_429" && !acc.Enabled && acc.BanTime > 0 {
			// 计算恢复时间（封禁时间 + 6 小时）
			banTime := time.Unix(acc.BanTime, 0)
			enableTime := banTime.Add(6 * time.Hour)

			if now.Before(enableTime) {
				// 还未到恢复时间，重新调度
				fmt.Printf("[AccountPool] Restoring recovery timer for account %s, will enable at %s\n",
					acc.Email, enableTime.Format("2006-01-02 15:04:05"))
				p.scheduleAutoEnable(acc.ID, enableTime)
			} else {
				// 已经过了恢复时间，立即恢复
				fmt.Printf("[AccountPool] Account %s recovery time passed during downtime, enabling now\n", acc.Email)
				p.autoEnableAccount(acc.ID)
			}
		}
	}
}

// autoEnableAccount 自动重新启用被限流禁用的账号（仅限 429）
func (p *AccountPool) autoEnableAccount(id string) {
	accounts := config.GetAccounts()
	for i, acc := range accounts {
		if acc.ID == id && acc.BanStatus == "RATE_LIMITED_429" {
			accounts[i].Enabled = true
			accounts[i].BanStatus = ""
			accounts[i].BanReason = ""
			accounts[i].BanTime = 0

			if err := config.UpdateAccount(id, accounts[i]); err == nil {
				fmt.Printf("[AccountPool] ✅ Account %s auto-enabled after 6-hour cooldown (429 recovered)\n", acc.Email)
				// 重新加载账号池
				p.Reload()
			} else {
				fmt.Printf("[AccountPool] Failed to auto-enable account %s: %v\n", acc.Email, err)
			}
			break
		} else if acc.ID == id && acc.BanStatus == "ACCOUNT_BANNED_403" {
			// 403 封禁的账号不自动恢复
			fmt.Printf("[AccountPool] Account %s is banned (403), skipping auto-enable\n", acc.Email)
			break
		}
	}
}

// tryRefreshToken 尝试刷新账号的 Token
// 注意：此方法在持有读锁时调用，不能修改 pool 状态
func (p *AccountPool) tryRefreshToken(acc *config.Account) *config.Account {
	if acc.RefreshToken == "" {
		fmt.Printf("[AccountPool] Account %s has no refresh token, skipping\n", acc.Email)
		return nil
	}

	fmt.Printf("[AccountPool] Attempting to refresh token for account %s (expires at %d)\n", 
		acc.Email, acc.ExpiresAt)

	// 调用刷新 Token 的方法
	newAccessToken, newRefreshToken, newExpiresAt, err := auth.RefreshToken(acc)
	if err != nil {
		fmt.Printf("[AccountPool] ❌ Failed to refresh token for account %s: %v\n", acc.Email, err)
		return nil
	}

	// 更新账号信息（需要在配置文件中持久化）
	updatedAccount := *acc
	updatedAccount.AccessToken = newAccessToken
	if newRefreshToken != "" {
		updatedAccount.RefreshToken = newRefreshToken
	}
	updatedAccount.ExpiresAt = newExpiresAt

	// 保存到配置文件
	if err := config.UpdateAccount(acc.ID, updatedAccount); err != nil {
		fmt.Printf("[AccountPool] ❌ Failed to save refreshed token for account %s: %v\n", acc.Email, err)
		return nil
	}

	// 更新内存中的账号池（需要异步重新加载）
	go func() {
		time.Sleep(100 * time.Millisecond)
		p.Reload()
	}()

	fmt.Printf("[AccountPool] ✅ Token refreshed for account %s, new expiry: %s\n", 
		acc.Email, time.Unix(newExpiresAt, 0).Format("2006-01-02 15:04:05"))

	return &updatedAccount
}
