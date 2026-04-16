# Kiro-Go 防封号机制实现文档

## 📋 概述

本文档描述了 Kiro-Go 项目中实现的四大防封号核心机制，用于防止 AWS 账号因高并发、超长上下文、频繁 429 等问题被封禁。

---

## ✅ 已实现的核心功能

### 1. 强制串行化的排队机制 (Request Queueing & Concurrency Control)

**问题：** 前端 Agent 瞬间发起几十个并发请求，直接打穿 AWS 限制。

**解决方案：**
- 使用 `golang.org/x/sync/semaphore` 实现全局并发控制
- 默认并发度为 1（完全串行化）
- 超出并发的请求在 Go 协程中排队等待（Block & Wait）
- 支持 60 秒排队超时保护
- 监听客户端断开（`ctx.Done()`）

**相关文件：**
- `proxy/handler.go` - 实现 `callKiroAPIWithQueue` 方法
- `config/config.go` - 添加配置项

**配置示例：**
```json
{
  "concurrencyLimit": 1,  // 最大并发数（1=完全串行，2=允许2个并发）
  "queueTimeout": 60      // 排队超时时间（秒）
}
```

**工作流程：**
```
请求1 → 立即执行
请求2 → 排队等待（阻塞）
请求3 → 排队等待（阻塞）
...
请求1 完成 → 请求2 开始执行
请求2 完成 → 请求3 开始执行
```

---

### 2. 会话粘滞与 TTL 释放 (Session Affinity & Idle Release)

**问题：** 
- 账号分配混乱导致 AWS 识别到"精神分裂"的会话上下文
- 没有释放机制导致账号死锁

**解决方案：**
- 维护 `前端 ConversationID → AWS 账号` 映射
- 10 分钟闲置超时（TTL）
- Keep-Alive 机制（有新请求时重置 TTL）
- 会话超时时清理 AWS ConversationID（防止数据串号）

**相关文件：**
- `pool/account_enhanced.go` - 增强版账号池实现

**核心 API：**
```go
// 获取会话绑定的账号（自动 Keep-Alive）
account := pool.GetForConversation(conversationID)

// 绑定 AWS ConversationID
pool.BindAwsConversation(accountID, awsConvID)

// 获取 AWS ConversationID（空字符串表示需要新建对话）
awsConvID := pool.GetAwsConversation(accountID)

// 清除 AWS ConversationID（会话超时时调用）
pool.ClearAwsConversation(accountID)
```

**工作流程：**
```
1. 前端发起请求（conversationID: "abc123"）
2. 检查是否已绑定账号
   - 已绑定 → 返回绑定的账号，重置 TTL
   - 未绑定 → 分配新账号，建立绑定
3. 10 分钟内有新请求 → 重置 TTL（Keep-Alive）
4. 10 分钟无请求 → 自动清理绑定和 AWS ConversationID
```

**安全保证：**
- 会话超时时，彻底清理账号的 AWS ConversationID
- 下次使用该账号时，会触发新建对话
- 防止数据串号泄露

---

### 3. 上下文截断保护 (Context Truncation)

**问题：** 老用户超时后回归，分到新账号，前端把几十分钟的超长历史一次性发过来，导致新账号被秒封。

**解决方案：**
- 检测是否是新账号接手老对话
- 实施滑动窗口截断：仅保留最近 5 轮对话
- 确保转换后的 Payload 控制在 3500 Token 以内

**相关文件：**
- `proxy/context_truncator.go` - 上下文截断实现

**核心 API：**
```go
// 检测是否是新会话
isNewSession := IsNewSessionForAccount(accountID, pool.GetAwsConversation)

// 截断 Claude 消息
truncated := TruncateClaudeMessages(messages, isNewSession)

// 截断 OpenAI 消息
truncated := TruncateOpenAIMessages(messages, isNewSession)
```

**截断策略：**
- 保留最近 5 轮对话（user-assistant 对）
- 保留所有 system 消息
- 估算 Token 数：字符数 / 4
- 安全阈值：3500 Token

**工作流程：**
```
1. 检测账号是否有 AWS ConversationID
   - 有 → 老会话，不截断
   - 无 → 新会话，检查是否需要截断

2. 估算消息总 Token 数
   - ≤ 3500 Token → 不截断
   - > 3500 Token → 执行截断

3. 截断逻辑
   - 保留最近 5 轮对话
   - 丢弃更早的历史记录
   - 记录截断日志
```

---

### 4. 熔断器与指数退避 (Circuit Breaker & Exponential Backoff)

**问题：** 遇到 429 时无延迟死磕，导致账号被永久封禁。

**解决方案：**
- 连续 3 次 429 触发熔断（1 小时冷却）
- 指数退避：3秒 → 6秒 → 12秒
- 自动恢复机制
- 熔断时清理所有会话绑定

**相关文件：**
- `pool/account_enhanced.go` - 熔断器实现
- `proxy/kiro.go` - 退避重试逻辑（需要集成）

**核心 API：**
```go
// 记录成功（清除熔断状态）
pool.RecordSuccess(accountID)

// 记录错误（触发熔断逻辑）
pool.RecordError(accountID, isQuotaError)

// 获取退避时间
backoff := pool.GetBackoffDuration(accountID)
```

**熔断逻辑：**
```
第 1 次 429 → 冷却 1 小时（可配置）
第 2 次 429 → 冷却 1 小时
第 3 次 429 → 触发熔断！
  - 禁用账号 1 小时
  - 清理所有会话绑定
  - 清理 AWS ConversationID
  - 自动恢复定时器
```

**指数退避：**
```
第 1 次 429 → 等待 3 秒后重试
第 2 次 429 → 等待 6 秒后重试
第 3 次 429 → 等待 12 秒后重试（触发熔断）
```

---

## 🔧 集成指南

### 步骤 1：使用增强版账号池

在 `pool/account.go` 中，将原有的 `AccountPool` 替换为 `AccountPoolEnhanced`：

```go
// 修改 GetPool 函数
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPoolEnhanced{
			cooldowns:          make(map[string]time.Time),
			errorCounts:        make(map[string]int),
			timers:             make(map[string]*time.Timer),
			sessionAffinity:    make(map[string]string),
			sessionLastUsed:    make(map[string]time.Time),
			sessionAwsConvID:   make(map[string]string),
			consecutive429:     make(map[string]int),
			circuitBreakerTime: make(map[string]time.Time),
			last429Time:        make(map[string]time.Time),
		}
		pool.Reload()
		pool.restorePendingRecoveries()
		go pool.cleanupStaleSessions()
	})
	return pool
}
```

### 步骤 2：在请求转换时应用上下文截断

在 `proxy/handler.go` 的 `handleClaudeMessagesInternal` 中：

```go
func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	// ... 读取请求 ...
	
	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	
	// 获取账号
	conversationID := buildConversationIDWithMetadata(req.Metadata, req.Model, "", "")
	account := h.pool.GetForConversation(conversationID)
	if account == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}
	
	// 🆕 检测是否是新会话并截断上下文
	isNewSession := IsNewSessionForAccount(account.ID, h.pool.GetAwsConversation)
	if isNewSession {
		req.Messages = TruncateClaudeMessages(req.Messages, isNewSession)
	}
	
	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)
	
	// ... 继续处理 ...
}
```

### 步骤 3：在 AWS API 调用后绑定 ConversationID

在 `proxy/kiro.go` 的 `CallKiroAPIWithContext` 中：

```go
func CallKiroAPIWithContext(ctx interface{}, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	// ... 发起请求 ...
	
	// 成功后绑定 AWS ConversationID
	if err == nil {
		awsConvID := payload.ConversationState.ConversationID
		pool.GetPool().BindAwsConversation(account.ID, awsConvID)
	}
	
	return err
}
```

### 步骤 4：在重试逻辑中应用指数退避

在 `proxy/kiro.go` 的 `CallKiroAPIWithContext` 中：

```go
func CallKiroAPIWithContext(ctx interface{}, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	maxRetries := 2
	
	for retry := 0; retry < maxRetries; retry++ {
		// 🆕 获取退避时间
		if retry > 0 {
			backoff := pool.GetPool().GetBackoffDuration(account.ID)
			if backoff > 0 {
				fmt.Printf("[KiroAPI] Applying exponential backoff: %v\n", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.(interface{ Done() <-chan struct{} }).Done():
					return fmt.Errorf("request cancelled during backoff")
				}
			} else {
				// 默认 3 秒
				time.Sleep(3 * time.Second)
			}
		}
		
		// ... 发起请求 ...
		
		if resp.StatusCode == 429 {
			resp.Body.Close()
			
			// 🆕 记录 429 错误（触发熔断逻辑）
			pool.GetPool().RecordError(account.ID, true)
			
			if retry == maxRetries-1 {
				return fmt.Errorf("rate_limit:quota exhausted after %d retries", maxRetries)
			}
			continue
		}
		
		// 成功
		pool.GetPool().RecordSuccess(account.ID)
		return parseEventStream(resp.Body, callback)
	}
	
	return fmt.Errorf("all retries failed")
}
```

---

## 📊 监控与日志

### 关键日志输出

**会话粘滞：**
```
[SessionAffinity] ✓ Binding conversation abc123 to account user@example.com
[SessionAffinity] Existing session: Account user@example.com has AWS ConversationID xyz789
[SessionAffinity] 🧹 Cleaning up stale session: conversation=abc123, account=user@example.com
```

**上下文截断：**
```
[ContextTruncation] 🆕 NEW SESSION: Account user@example.com has no AWS ConversationID
[ContextTruncation] Original: 20 messages, ~8000 tokens
[ContextTruncation] Truncated: 10 messages, ~3200 tokens (kept last 5 rounds)
```

**熔断器：**
```
[AccountPool] Account user@example.com got 429 error (consecutive: 1/3)
[AccountPool] Account user@example.com got 429 error (consecutive: 2/3)
[AccountPool] 🔴 CIRCUIT BREAKER: Account user@example.com disabled due to 3 consecutive 429 errors
[AccountPool] ✅ Account user@example.com auto-enabled after cooldown (circuit breaker recovered)
```

**排队机制：**
```
[Handler] AWS concurrency limit: 1 (requests will queue if limit exceeded)
[Handler] Request queued, waiting for semaphore...
[Handler] Request acquired semaphore, executing...
```

---

## 🧪 测试建议

### 1. 测试并发控制
```bash
# 同时发起 10 个请求
for i in {1..10}; do
  curl -X POST http://localhost:8080/v1/messages \
    -H "Content-Type: application/json" \
    -d '{"model":"claude-sonnet-4","messages":[{"role":"user","content":"Hello"}]}' &
done
```

**预期结果：** 请求串行执行，不会同时向 AWS 发起多个请求

### 2. 测试会话粘滞
```bash
# 使用相同的 user_id 发起多个请求
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","user":"test-user-123","messages":[{"role":"user","content":"Hello"}]}'
```

**预期结果：** 相同 user_id 的请求绑定到同一个 AWS 账号

### 3. 测试上下文截断
```bash
# 发送超长历史记录
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model":"claude-sonnet-4",
    "messages":[
      {"role":"user","content":"Message 1..."},
      {"role":"assistant","content":"Response 1..."},
      ... (20+ rounds)
      {"role":"user","content":"Latest message"}
    ]
  }'
```

**预期结果：** 如果是新会话，只保留最近 5 轮对话

### 4. 测试熔断器
```bash
# 模拟连续 429 错误（需要修改代码或使用 mock）
# 预期结果：3 次 429 后账号被禁用 1 小时
```

---

## 📈 性能影响

### 并发控制
- **延迟增加：** 排队等待时间（取决于请求处理速度）
- **吞吐量：** 受 `concurrencyLimit` 限制
- **建议：** 生产环境可设置为 2-3，平衡性能和安全

### 会话粘滞
- **内存占用：** 每个会话约 100 字节
- **清理频率：** 每 2 分钟清理一次
- **影响：** 可忽略不计

### 上下文截断
- **CPU 开销：** Token 估算（字符串长度计算）
- **延迟：** < 1ms
- **影响：** 可忽略不计

### 熔断器
- **内存占用：** 每个账号约 50 字节
- **影响：** 可忽略不计

---

## 🔒 安全注意事项

1. **数据隔离：** 会话超时时必须清理 AWS ConversationID
2. **并发安全：** 所有 map 操作都使用 `sync.RWMutex` 保护
3. **资源泄露：** 排队超时防止 goroutine 泄露
4. **客户端断开：** 监听 `ctx.Done()` 及时释放资源

---

## 📚 参考资料

- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore)
- [AWS Rate Limiting Best Practices](https://docs.aws.amazon.com/general/latest/gr/api-retries.html)
- [Circuit Breaker Pattern](https://martinfowler.com/bliki/CircuitBreaker.html)

---

## 🎯 下一步优化建议

1. **动态并发调整：** 根据 429 频率自动调整 `concurrencyLimit`
2. **账号优先级：** 根据订阅类型（FREE/PRO/PRO_PLUS）分配不同的并发配额
3. **Token 精确计算：** 使用 tiktoken 库精确计算 Token 数
4. **分布式会话：** 使用 Redis 实现跨实例的会话粘滞
5. **监控面板：** 实时展示排队长度、熔断状态、会话数量等指标

---

**文档版本：** 1.0.0  
**最后更新：** 2024-01-XX  
**维护者：** Kiro-Go Team
