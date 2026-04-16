# Token 自动刷新功能

## 功能说明

当账号池中没有可用的账号时，系统会自动尝试刷新过期的 Token，确保服务的连续性。

## 工作原理

### 1. Token 过期检测

在 `pool.GetNext()` 方法中，系统会检查每个账号的 Token 是否即将过期（过期前 5 分钟）：

```go
if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-300 {
    // Token 即将过期，收集到待刷新列表
    if acc.RefreshToken != "" {
        expiredAccounts = append(expiredAccounts, acc)
    }
}
```

### 2. 自动刷新逻辑

当没有可用账号时，系统会自动尝试刷新收集到的过期账号：

```go
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
```

### 3. Token 刷新实现

`tryRefreshToken()` 方法负责实际的刷新操作：

1. 检查账号是否有 `RefreshToken`
2. 调用 `auth.RefreshToken()` 获取新的 Token
3. 更新账号信息并保存到配置文件
4. 异步重新加载账号池

```go
func (p *AccountPool) tryRefreshToken(acc *config.Account) *config.Account {
    if acc.RefreshToken == "" {
        return nil
    }

    // 调用刷新 Token 的方法
    newAccessToken, newRefreshToken, newExpiresAt, err := auth.RefreshToken(acc)
    if err != nil {
        fmt.Printf("[AccountPool] ❌ Failed to refresh token for account %s: %v\n", acc.Email, err)
        return nil
    }

    // 更新账号信息
    updatedAccount := *acc
    updatedAccount.AccessToken = newAccessToken
    if newRefreshToken != "" {
        updatedAccount.RefreshToken = newRefreshToken
    }
    updatedAccount.ExpiresAt = newExpiresAt

    // 保存到配置文件
    if err := config.UpdateAccount(acc.ID, updatedAccount); err != nil {
        return nil
    }

    // 异步重新加载账号池
    go func() {
        time.Sleep(100 * time.Millisecond)
        p.Reload()
    }()

    return &updatedAccount
}
```

## 支持的认证方式

Token 刷新支持以下认证方式：

### 1. OIDC/Builder ID Token

使用 AWS OIDC 端点刷新：

```
POST https://oidc.{region}.amazonaws.com/token
{
  "clientId": "...",
  "clientSecret": "...",
  "refreshToken": "...",
  "grantType": "refresh_token"
}
```

### 2. Social Token (GitHub/Google)

使用 Kiro 认证服务刷新：

```
POST https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken
{
  "refreshToken": "..."
}
```

## 日志输出

系统会输出详细的刷新日志：

```
[AccountPool] No available accounts, attempting to refresh 2 expired tokens
[AccountPool] Attempting to refresh token for account user@example.com (expires at 1234567890)
[AccountPool] ✅ Token refreshed for account user@example.com, new expiry: 2024-04-16 15:30:00
[AccountPool] ✅ Successfully refreshed token for account user@example.com
```

如果刷新失败：

```
[AccountPool] ❌ Failed to refresh token for account user@example.com: refresh failed: 400 invalid_grant
[AccountPool] ❌ Failed to save refreshed token for account user@example.com: config update error
```

## 优势

1. **无缝体验**：用户无需手动重新登录，系统自动维护 Token 有效性
2. **高可用性**：即使所有账号 Token 都过期，系统也会尝试自动恢复
3. **智能调度**：只在没有可用账号时才触发刷新，避免不必要的 API 调用
4. **持久化**：刷新后的 Token 会立即保存到配置文件，重启后依然有效

## 注意事项

1. **RefreshToken 必需**：只有包含 `RefreshToken` 的账号才能自动刷新
2. **刷新失败处理**：如果刷新失败（如 RefreshToken 也过期），账号会被跳过
3. **并发安全**：刷新操作在读锁下进行，不会阻塞其他请求
4. **异步重载**：刷新成功后会异步重新加载账号池，确保新 Token 立即可用

## 配置要求

确保账号配置包含以下字段：

```json
{
  "id": "account-id",
  "email": "user@example.com",
  "accessToken": "eyJ...",
  "refreshToken": "eyJ...",
  "expiresAt": 1234567890,
  "clientId": "xxx",
  "clientSecret": "xxx",
  "region": "us-east-1",
  "authMethod": "oidc"
}
```

其中：
- `refreshToken`: 必需，用于刷新 AccessToken
- `expiresAt`: 必需，Token 过期时间戳（秒）
- `clientId` / `clientSecret`: OIDC 认证必需
- `region`: OIDC 认证必需，默认 `us-east-1`
- `authMethod`: 认证方式，`oidc` 或 `social`
