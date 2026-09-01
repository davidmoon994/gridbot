// Package store 提供基于 SQLite 的持久化存储：
//   - 网格配置（重启后可恢复）
//   - 事件/日志（网格成交、重新居中、风控拒单等）
//   - 已实现盈亏历史（用于统计和图表）
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 封装数据库连接
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）SQLite 数据库文件并初始化表结构
//
// 使用 modernc.org/sqlite（纯 Go 实现，不依赖 CGO/gcc），
// 这样在 Windows 上无需安装任何 C 编译器即可直接编译运行。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS grid_configs (
			symbol TEXT PRIMARY KEY,
			config_json TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			symbol TEXT NOT NULL,
			event_type TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_symbol_time ON events(symbol, created_at)`,
		`CREATE TABLE IF NOT EXISTS admin_account (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			salt TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS exchange_credentials (
			exchange_type TEXT PRIMARY KEY,
			api_key TEXT NOT NULL,
			api_secret TEXT NOT NULL,
			passphrase TEXT NOT NULL DEFAULT '',
			quote_asset TEXT NOT NULL DEFAULT 'USDT',
			testnet INTEGER NOT NULL DEFAULT 0,
			hedge_mode INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS active_exchange (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			exchange_type TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pnl_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			symbol TEXT NOT NULL,
			realized_pnl REAL NOT NULL,
			total_position_quote REAL NOT NULL,
			created_at DATETIME NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate failed: %w", err)
		}
	}
	return nil
}

// SaveGridConfig 保存/更新某个交易对的网格配置
func (s *Store) SaveGridConfig(symbol string, cfg interface{}, enabled bool) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO grid_configs (symbol, config_json, enabled, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(symbol) DO UPDATE SET config_json=excluded.config_json, enabled=excluded.enabled, updated_at=excluded.updated_at
	`, symbol, string(b), boolToInt(enabled), time.Now())
	return err
}

// LoadGridConfigs 加载所有已保存的网格配置（重启恢复用）
func (s *Store) LoadGridConfigs() (map[string]json.RawMessage, map[string]bool, error) {
	rows, err := s.db.Query(`SELECT symbol, config_json, enabled FROM grid_configs`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	configs := map[string]json.RawMessage{}
	enabled := map[string]bool{}
	for rows.Next() {
		var symbol, cfgJSON string
		var en int
		if err := rows.Scan(&symbol, &cfgJSON, &en); err != nil {
			return nil, nil, err
		}
		configs[symbol] = json.RawMessage(cfgJSON)
		enabled[symbol] = en != 0
	}
	return configs, enabled, nil
}

// LogEvent 写入一条事件日志
func (s *Store) LogEvent(symbol, eventType, message string, t time.Time) error {
	_, err := s.db.Exec(`INSERT INTO events (symbol, event_type, message, created_at) VALUES (?, ?, ?, ?)`,
		symbol, eventType, message, t)
	return err
}

// EventRecord 事件记录（查询返回用）
type EventRecord struct {
	ID        int64     `json:"id"`
	Symbol    string    `json:"symbol"`
	EventType string    `json:"event_type"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// RecentEvents 查询某交易对最近的事件（limit条），symbol为空表示查所有
func (s *Store) RecentEvents(symbol string, limit int) ([]EventRecord, error) {
	var rows *sql.Rows
	var err error
	if symbol == "" {
		rows, err = s.db.Query(`SELECT id, symbol, event_type, message, created_at FROM events ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(`SELECT id, symbol, event_type, message, created_at FROM events WHERE symbol = ? ORDER BY id DESC LIMIT ?`, symbol, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventRecord
	for rows.Next() {
		var r EventRecord
		if err := rows.Scan(&r.ID, &r.Symbol, &r.EventType, &r.Message, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RecordPnLSnapshot 记录一次盈亏快照，用于绘制历史曲线
func (s *Store) RecordPnLSnapshot(symbol string, realizedPnL, totalPositionQuote float64, t time.Time) error {
	_, err := s.db.Exec(`INSERT INTO pnl_history (symbol, realized_pnl, total_position_quote, created_at) VALUES (?, ?, ?, ?)`,
		symbol, realizedPnL, totalPositionQuote, t)
	return err
}

// PnLPoint 盈亏历史点
type PnLPoint struct {
	RealizedPnL        float64   `json:"realized_pnl"`
	TotalPositionQuote float64   `json:"total_position_quote"`
	CreatedAt          time.Time `json:"created_at"`
}

// PnLHistory 查询盈亏历史
func (s *Store) PnLHistory(symbol string, limit int) ([]PnLPoint, error) {
	rows, err := s.db.Query(`SELECT realized_pnl, total_position_quote, created_at FROM pnl_history WHERE symbol = ? ORDER BY id DESC LIMIT ?`, symbol, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PnLPoint
	for rows.Next() {
		var p PnLPoint
		if err := rows.Scan(&p.RealizedPnL, &p.TotalPositionQuote, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- 管理员账户 ----

// HasAdminAccount 判断是否已创建过管理账户
func (s *Store) HasAdminAccount() (bool, error) {
	var cnt int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_account WHERE id = 1`).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// CreateAdminAccount 创建唯一的管理账户（id固定为1，重复创建会因主键冲突失败）
func (s *Store) CreateAdminAccount(username, passwordHash, salt string) error {
	_, err := s.db.Exec(`INSERT INTO admin_account (id, username, password_hash, salt, created_at) VALUES (1, ?, ?, ?, ?)`,
		username, passwordHash, salt, time.Now())
	return err
}

// GetAdminAccount 读取管理账户信息；不存在时返回空字符串（不视为错误）
func (s *Store) GetAdminAccount() (username, passwordHash, salt string, err error) {
	row := s.db.QueryRow(`SELECT username, password_hash, salt FROM admin_account WHERE id = 1`)
	err = row.Scan(&username, &passwordHash, &salt)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	return username, passwordHash, salt, err
}

// UpdateAdminPassword 更新管理账户密码
func (s *Store) UpdateAdminPassword(passwordHash, salt string) error {
	_, err := s.db.Exec(`UPDATE admin_account SET password_hash = ?, salt = ? WHERE id = 1`, passwordHash, salt)
	return err
}

// ---- 交易所凭证（API Key 绑定/解绑） ----

// ExchangeCredential 是一组交易所连接凭证
type ExchangeCredential struct {
	ExchangeType string `json:"exchange_type"` // "binance_futures" | "okx"
	APIKey       string `json:"api_key"`
	APISecret    string `json:"api_secret"`
	Passphrase   string `json:"passphrase"` // 仅 OKX 需要
	QuoteAsset   string `json:"quote_asset"`
	Testnet      bool   `json:"testnet"`
	HedgeMode    bool   `json:"hedge_mode"`
}

// SaveCredential 保存/更新某个交易所的凭证（绑定）
func (s *Store) SaveCredential(c ExchangeCredential) error {
	_, err := s.db.Exec(`
		INSERT INTO exchange_credentials (exchange_type, api_key, api_secret, passphrase, quote_asset, testnet, hedge_mode, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(exchange_type) DO UPDATE SET
			api_key=excluded.api_key, api_secret=excluded.api_secret, passphrase=excluded.passphrase,
			quote_asset=excluded.quote_asset, testnet=excluded.testnet, hedge_mode=excluded.hedge_mode,
			updated_at=excluded.updated_at
	`, c.ExchangeType, c.APIKey, c.APISecret, c.Passphrase, c.QuoteAsset, boolToInt(c.Testnet), boolToInt(c.HedgeMode), time.Now())
	return err
}

// DeleteCredential 删除某个交易所的凭证（解绑）
func (s *Store) DeleteCredential(exchangeType string) error {
	_, err := s.db.Exec(`DELETE FROM exchange_credentials WHERE exchange_type = ?`, exchangeType)
	return err
}

// GetCredential 读取某个交易所的凭证；不存在时返回 (nil, nil)
func (s *Store) GetCredential(exchangeType string) (*ExchangeCredential, error) {
	row := s.db.QueryRow(`SELECT exchange_type, api_key, api_secret, passphrase, quote_asset, testnet, hedge_mode
		FROM exchange_credentials WHERE exchange_type = ?`, exchangeType)
	var c ExchangeCredential
	var testnet, hedge int
	err := row.Scan(&c.ExchangeType, &c.APIKey, &c.APISecret, &c.Passphrase, &c.QuoteAsset, &testnet, &hedge)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Testnet = testnet != 0
	c.HedgeMode = hedge != 0
	return &c, nil
}

// ListCredentials 列出所有已绑定的交易所凭证
func (s *Store) ListCredentials() ([]ExchangeCredential, error) {
	rows, err := s.db.Query(`SELECT exchange_type, api_key, api_secret, passphrase, quote_asset, testnet, hedge_mode FROM exchange_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExchangeCredential
	for rows.Next() {
		var c ExchangeCredential
		var testnet, hedge int
		if err := rows.Scan(&c.ExchangeType, &c.APIKey, &c.APISecret, &c.Passphrase, &c.QuoteAsset, &testnet, &hedge); err != nil {
			return nil, err
		}
		c.Testnet = testnet != 0
		c.HedgeMode = hedge != 0
		out = append(out, c)
	}
	return out, nil
}

// SetActiveExchange 记录当前生效的交易所标识，重启后据此自动恢复连接
func (s *Store) SetActiveExchange(exchangeType string) error {
	_, err := s.db.Exec(`
		INSERT INTO active_exchange (id, exchange_type, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET exchange_type=excluded.exchange_type, updated_at=excluded.updated_at
	`, exchangeType, time.Now())
	return err
}

// GetActiveExchange 读取当前生效的交易所标识；未设置时返回空字符串
func (s *Store) GetActiveExchange() (string, error) {
	var t string
	err := s.db.QueryRow(`SELECT exchange_type FROM active_exchange WHERE id = 1`).Scan(&t)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return t, err
}
