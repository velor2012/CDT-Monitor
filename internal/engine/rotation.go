package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wang4386/CDT-Monitor/internal/domain"
	"github.com/wang4386/CDT-Monitor/internal/notify"
)

const JobRotatePool = "rotate_pool"

// rotateWindow matches the daily-schedule compensation window: a rotation that
// became due while the process was down still fires within 10 minutes.
const rotateWindow = 10 * time.Minute

// ipPollInterval and ipPollAttempts implement the confirmed IP wait policy:
// poll every 10s for up to ~2 minutes after a start command.
const ipPollInterval = 10 * time.Second
const ipPollAttempts = 12

// checkRotationPools enqueues one rotate_pool job per eligible pool. It runs
// inside the existing 15s scheduler loop; no new timer or framework.
func (e *Engine) checkRotationPools(ctx context.Context, now time.Time) {
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("rotation config load failed", "error", err)
		}
		return
	}
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	localNow := now.In(location)
	if !dueWithin(localNow, config.RotationTime, rotateWindow) {
		return
	}
	pools, err := e.store.ListRotationPools(ctx)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("rotation pools load failed", "error", err)
		}
		return
	}
	for _, pool := range pools {
		if !pool.Enabled || len(pool.AccountIDs) < 2 {
			continue
		}
		state, err := e.store.RotationPoolState(ctx, pool.ID)
		if err != nil {
			continue
		}
		if state.LastRotateDate == localNow.Format("2006-01-02") {
			continue
		}
		payload := fmt.Sprintf(`{"pool_id":%d}`, pool.ID)
		uniqueKey := fmt.Sprintf("rotate:%d:%s", pool.ID, localNow.Format("20060102"))
		if _, err = e.Enqueue(ctx, JobRotatePool, 0, payload, uniqueKey); err != nil {
			if e.logger != nil {
				e.logger.Warn("enqueue rotation job failed", "pool", pool.ID, "error", err)
			}
		}
	}
}

// runRotation executes one pool rotation: start next instance, wait for its
// public IP, bind the pool domain, then stop the remaining instances.
func (e *Engine) runRotation(ctx context.Context, poolID int64) (string, error) {
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		return "", err
	}
	pools, err := e.store.ListRotationPools(ctx)
	if err != nil {
		return "", err
	}
	var pool *domain.RotationPool
	for index := range pools {
		if pools[index].ID == poolID {
			pool = &pools[index]
			break
		}
	}
	if pool == nil || !pool.Enabled || len(pool.AccountIDs) < 2 {
		return "pool not eligible for rotation", nil
	}
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	now := time.Now().In(location)
	today := now.Format("2006-01-02")

	accounts := make([]domain.Account, 0, len(pool.AccountIDs))
	byID := make(map[int64]domain.Account, len(config.Accounts))
	for _, account := range config.Accounts {
		byID[account.ID] = account
	}
	for _, id := range pool.AccountIDs {
		if account, found := byID[id]; found {
			accounts = append(accounts, account)
		}
	}
	if len(accounts) < 2 {
		return "pool has fewer than two live instances", nil
	}
	for _, account := range accounts {
		if account.ScheduleEnabled {
			_ = e.store.AddLog(ctx, "error", fmt.Sprintf("轮转池 [%s] 实例 [%s] 已开启每日定时开关机，跳过本次轮转", pool.Name, account.Remark))
			return "", errors.New("rotation pool contains an instance with daily schedule enabled")
		}
	}

	state, err := e.store.RotationPoolState(ctx, pool.ID)
	if err != nil {
		return "", err
	}
	next := nextRotationTarget(accounts, state, today)
	if next.ID == state.CurrentAccountID && state.LastRotateDate != "" {
		return fmt.Sprintf("rotation already active on account %d", next.ID), nil
	}

	token := ""
	if pool.DDNSEnabled {
		token, err = e.store.RotationPoolToken(ctx, pool.ID)
		if err != nil {
			return "", err
		}
		if token == "" {
			e.rotationFailure(ctx, config, pool, next, "Cloudflare API Token 未配置")
			return "", errors.New("rotation pool token missing")
		}
	}

	secret, err := e.store.AccountSecret(ctx, next.ID)
	if err != nil {
		return "", err
	}
	// Start the next instance first; a failed start aborts the switch.
	if err = e.provider.ControlInstance(ctx, next, secret, "start", "KeepCharging"); err != nil {
		e.rotationFailure(ctx, config, pool, next, fmt.Sprintf("开机失败: %v", err))
		return "", fmt.Errorf("rotate start account %d: %w", next.ID, err)
	}
	_ = e.store.UpdateRuntime(ctx, next.ID, next.TrafficUsed, domain.StatusStarting, time.Now().UTC())
	_ = e.store.AddLog(ctx, "info", fmt.Sprintf("轮转池 [%s] 开机实例 [%s]", pool.Name, rotationLabel(next)))

	ip := ""
	if pool.DDNSEnabled {
		for attempt := range ipPollAttempts {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(ipPollInterval):
				}
			}
			ip, err = e.provider.GetInstancePublicIP(ctx, next, secret)
			if err == nil && ip != "" {
				break
			}
			if err != nil {
				ip = ""
			}
		}
		if ip == "" {
			e.rotationFailure(ctx, config, pool, next, "开机后未能获取公网 IPv4（已等待约 2 分钟）")
			return "", errors.New("rotation public IP unavailable")
		}
		cf := notify.NewCloudflareClient()
		recordName := poolDomain(pool)
		if err = cf.SyncARecord(ctx, token, pool.CFZoneID, pool.RootDomain, recordName, ip, pool.Proxied); err != nil {
			e.rotationFailure(ctx, config, pool, next, fmt.Sprintf("DDNS 更新失败: %v", err))
			return "", fmt.Errorf("rotate ddns account %d: %w", next.ID, err)
		}
		_ = e.store.AddLog(ctx, "info", fmt.Sprintf("轮转池 [%s] 域名 %s 已绑定 %s", pool.Name, recordName, ip))
	} else {
		_ = e.store.AddLog(ctx, "info", fmt.Sprintf("轮转池 [%s] DDNS 未启用，跳过域名绑定", pool.Name))
	}

	// DNS is bound: stop every other pool instance.
	for _, account := range accounts {
		if account.ID == next.ID {
			continue
		}
		if account.InstanceStatus != domain.StatusRunning && account.InstanceStatus != domain.StatusStarting {
			continue
		}
		stopSecret, stopErr := e.store.AccountSecret(ctx, account.ID)
		if stopErr != nil {
			continue
		}
		if stopErr = e.provider.ControlInstance(ctx, account, stopSecret, "stop", resolveShutdownMode(account, config)); stopErr != nil {
			_ = e.store.AddLog(ctx, "error", fmt.Sprintf("轮转池 [%s] 关机实例 [%s] 失败: %v", pool.Name, rotationLabel(account), stopErr))
			continue
		}
		_ = e.store.UpdateRuntime(ctx, account.ID, account.TrafficUsed, domain.StatusStopping, time.Now().UTC())
		_ = e.store.AddLog(ctx, "info", fmt.Sprintf("轮转池 [%s] 关机实例 [%s]", pool.Name, rotationLabel(account)))
	}

	if err = e.store.UpdateRotationPoolState(ctx, pool.ID, next.ID, today); err != nil {
		return "", err
	}
	return fmt.Sprintf("rotated pool %d to account %d (%s)", pool.ID, next.ID, ip), nil
}

// nextRotationTarget picks today's active instance: advance one position per
// elapsed day since the last rotation, so restarts stay continuous.
func nextRotationTarget(accounts []domain.Account, state domain.RotationPoolState, today string) domain.Account {
	index := 0
	if state.LastRotateDate != "" && state.CurrentAccountID != 0 {
		for position, account := range accounts {
			if account.ID == state.CurrentAccountID {
				index = position
				break
			}
		}
		last, err := time.ParseInLocation("2006-01-02", state.LastRotateDate, time.UTC)
		if err == nil {
			current, parseErr := time.ParseInLocation("2006-01-02", today, time.UTC)
			if parseErr == nil {
				days := int(current.Sub(last).Hours() / 24)
				if days > 0 {
					index = (index + days) % len(accounts)
				} else if days < 0 {
					// clock moved backwards: keep position, only advance one
					index = (index + 1) % len(accounts)
				}
			}
		}
	}
	return accounts[index]
}

func (e *Engine) rotationFailure(ctx context.Context, config domain.Config, pool *domain.RotationPool, next domain.Account, reason string) {
	_ = e.store.AddLog(ctx, "error", fmt.Sprintf("轮转池 [%s] 切换失败（实例 [%s]）: %s", pool.Name, rotationLabel(next), reason))
	event := newEvent("rotation", "轮转池切换失败", fmt.Sprintf("轮转池 [%s] 切换到实例 [%s] 失败：%s。旧实例已保持运行。", pool.Name, rotationLabel(next), reason), next.ID, map[string]string{
		"轮转池": pool.Name,
		"域名":  poolDomain(pool),
		"实例":  rotationLabel(next),
		"原因":  reason,
	})
	_ = e.store.AddOutbox(ctx, event, notify.EnabledChannels(config))
}

func rotationLabel(account domain.Account) string {
	if strings.TrimSpace(account.Remark) != "" {
		return account.Remark
	}
	return account.InstanceID
}

// poolDomain composes the A-record name: prefix.root when a prefix is set,
// otherwise the root domain itself.
func poolDomain(pool *domain.RotationPool) string {
	if pool.DomainPrefix == "" {
		return pool.RootDomain
	}
	return pool.DomainPrefix + "." + pool.RootDomain
}
