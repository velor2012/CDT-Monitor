package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wang4386/CDT-Monitor/internal/domain"
)

// ListRotationPools returns all non-deleted rotation pools with decrypted-token flags.
func (s *Store) ListRotationPools(ctx context.Context) ([]domain.RotationPool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, cf_token, cf_zone_id, root_domain, domain_prefix, proxied, ddns_enabled, enabled, account_ids FROM rotation_pools WHERE deleted_at=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pools := make([]domain.RotationPool, 0)
	for rows.Next() {
		var pool domain.RotationPool
		var token, accountIDs string
		var proxied, ddnsEnabled, enabled int
		if err = rows.Scan(&pool.ID, &pool.Name, &token, &pool.CFZoneID, &pool.RootDomain, &pool.DomainPrefix, &proxied, &ddnsEnabled, &enabled, &accountIDs); err != nil {
			return nil, err
		}
		pool.Proxied = proxied == 1
		pool.DDNSEnabled = ddnsEnabled == 1
		pool.Enabled = enabled == 1
		pool.TokenConfigured = token != ""
		if err = json.Unmarshal([]byte(accountIDs), &pool.AccountIDs); err != nil {
			return nil, fmt.Errorf("rotation pool %d account_ids: %w", pool.ID, err)
		}
		pools = append(pools, pool)
	}
	return pools, rows.Err()
}

// RotationPoolToken decrypts and returns the stored Cloudflare token for a pool.
func (s *Store) RotationPoolToken(ctx context.Context, poolID int64) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `SELECT cf_token FROM rotation_pools WHERE id=? AND deleted_at=0`, poolID).Scan(&token)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", nil
	}
	return s.Decrypt(token)
}

// saveRotationPoolsTx replaces all pools inside the config-save transaction.
// Token semantics match account secrets: empty token keeps the stored value.
func saveRotationPoolsTx(ctx context.Context, tx *sql.Tx, s *Store, pools []domain.RotationPool) error {
	type existing struct {
		id, token string
	}
	byID := map[int64]existing{}
	rows, err := tx.QueryContext(ctx, `SELECT id, cf_token FROM rotation_pools WHERE deleted_at=0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var row existing
		if err = rows.Scan(&id, &row.token); err != nil {
			rows.Close()
			return err
		}
		row.id = fmt.Sprint(id)
		byID[id] = row
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	// Pool membership references persisted account ids; resolve schedule flags
	// from the DB (config.Accounts may carry id=0 for not-yet-inserted rows).
	scheduleEnabled := map[int64]bool{}
	scheduleRows, err := tx.QueryContext(ctx, `SELECT id, schedule_enabled FROM accounts WHERE deleted_at=0`)
	if err != nil {
		return err
	}
	for scheduleRows.Next() {
		var id int64
		var enabled int
		if err = scheduleRows.Scan(&id, &enabled); err != nil {
			scheduleRows.Close()
			return err
		}
		scheduleEnabled[id] = enabled == 1
	}
	scheduleRows.Close()
	if err = scheduleRows.Err(); err != nil {
		return err
	}

	pooled := map[int64]bool{}
	kept := map[int64]bool{}
	for _, pool := range pools {
		name := strings.TrimSpace(pool.Name)
		if name == "" {
			return errors.New("rotation pool name is required")
		}
		rootDomain := strings.ToLower(strings.TrimSpace(pool.RootDomain))
		rootDomain = strings.TrimPrefix(strings.TrimPrefix(rootDomain, "https://"), "http://")
		rootDomain = strings.Trim(strings.Split(rootDomain, "/")[0], ".")
		if rootDomain == "" || !strings.Contains(rootDomain, ".") {
			return fmt.Errorf("轮转池 [%s] 根域名无效，例如 example.com", name)
		}
		domainPrefix := strings.ToLower(strings.TrimSpace(pool.DomainPrefix))
		domainPrefix = strings.Trim(domainPrefix, ".-")
		if domainPrefix != "" && strings.ContainsAny(domainPrefix, "./ ") {
			return fmt.Errorf("轮转池 [%s] 域名前缀无效，仅允许字母数字与连字符", name)
		}
		domain := domainPrefix
		if domain != "" {
			domain += "." + rootDomain
		} else {
			domain = rootDomain
		}
		seen := map[int64]bool{}
		ids := make([]int64, 0, len(pool.AccountIDs))
		for _, id := range pool.AccountIDs {
			if id <= 0 || seen[id] {
				continue
			}
			seen[id] = true
			if pooled[id] {
				return fmt.Errorf("实例 [%d] 已属于其他轮转池", id)
			}
			if scheduleEnabled[id] {
				var previouslyPooled int
				if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rotation_pools WHERE deleted_at=0 AND EXISTS (SELECT 1 FROM json_each(rotation_pools.account_ids) WHERE json_each.value = ?)`, id).Scan(&previouslyPooled); err != nil {
					return err
				}
				if previouslyPooled > 0 {
					return fmt.Errorf("实例 [%d] 已在轮转池中，不能开启每日定时开关机", id)
				}
				return fmt.Errorf("实例 [%d] 已开启每日定时开关机，不能加入轮转池", id)
			}
			pooled[id] = true
			ids = append(ids, id)
		}
		if pool.Enabled && len(ids) == 0 {
			return fmt.Errorf("轮转池 [%s] 启用前至少需要关联一个实例", name)
		}
		idsJSON, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		token := strings.TrimSpace(pool.CFToken)
		if row, found := byID[pool.ID]; found {
			if token == "" {
				token = row.token
			} else if !strings.HasPrefix(token, "enc:") {
				var encrypted string
				if encrypted, err = s.Encrypt(token); err != nil {
					return err
				}
				token = encrypted
			}
			if _, err = tx.ExecContext(ctx, `UPDATE rotation_pools SET name=?, cf_token=?, cf_zone_id=?, domain=?, root_domain=?, domain_prefix=?, proxied=?, ddns_enabled=?, enabled=?, account_ids=?, deleted_at=0 WHERE id=?`,
				name, token, strings.TrimSpace(pool.CFZoneID), domain, rootDomain, domainPrefix, boolInt(pool.Proxied), boolInt(pool.DDNSEnabled), boolInt(pool.Enabled), string(idsJSON), pool.ID); err != nil {
				return err
			}
			kept[pool.ID] = true
		} else {
			encrypted := ""
			if token != "" {
				if encrypted, err = s.Encrypt(token); err != nil {
					return err
				}
			}
			result, err := tx.ExecContext(ctx, `INSERT INTO rotation_pools(name,cf_token,cf_zone_id,domain,root_domain,domain_prefix,proxied,ddns_enabled,enabled,account_ids) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				name, encrypted, strings.TrimSpace(pool.CFZoneID), domain, rootDomain, domainPrefix, boolInt(pool.Proxied), boolInt(pool.DDNSEnabled), boolInt(pool.Enabled), string(idsJSON))
			if err != nil {
				return err
			}
			var id int64
			if id, err = result.LastInsertId(); err != nil {
				return err
			}
			kept[id] = true
		}
	}
	for id := range byID {
		if !kept[id] {
			if _, err = tx.ExecContext(ctx, `UPDATE rotation_pools SET deleted_at=unixepoch() WHERE id=?`, id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM rotation_pool_states WHERE pool_id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// RotationPoolState loads the persisted rotation state for one pool.
func (s *Store) RotationPoolState(ctx context.Context, poolID int64) (domain.RotationPoolState, error) {
	var state domain.RotationPoolState
	state.PoolID = poolID
	err := s.db.QueryRowContext(ctx, `SELECT current_account_id, last_rotate_date FROM rotation_pool_states WHERE pool_id=?`, poolID).
		Scan(&state.CurrentAccountID, &state.LastRotateDate)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	return state, err
}

// UpdateRotationPoolState persists rotation progress for one pool.
func (s *Store) UpdateRotationPoolState(ctx context.Context, poolID int64, currentAccountID int64, lastRotateDate string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO rotation_pool_states(pool_id,current_account_id,last_rotate_date) VALUES(?,?,?)
ON CONFLICT(pool_id) DO UPDATE SET current_account_id=excluded.current_account_id, last_rotate_date=excluded.last_rotate_date`,
		poolID, currentAccountID, lastRotateDate)
	return err
}

// AccountInRotationPool reports whether the account is a member of any active pool.
func (s *Store) AccountInRotationPool(ctx context.Context, accountID int64) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rotation_pools WHERE deleted_at=0 AND EXISTS (SELECT 1 FROM json_each(rotation_pools.account_ids) WHERE json_each.value = ?)`, accountID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// RotationPoolTokenByDomain returns the stored token for the pool whose
// root domain matches, so unsaved editor state can reuse the saved token.
func (s *Store) RotationPoolTokenByDomain(ctx context.Context, rootDomain string) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `SELECT cf_token FROM rotation_pools WHERE deleted_at=0 AND root_domain=? ORDER BY id LIMIT 1`, rootDomain).Scan(&token)
	if err != nil {
		return "", nil
	}
	if token == "" {
		return "", nil
	}
	return s.Decrypt(token)
}
