// Package store 提供基于 SQLite 的持久化存储：
//   - 网格配置（重启后可恢复）
//   - 事件/日志（网格成交、重新居中、风控拒单等）
//   - 已实现盈亏历史（用于统计和图表）
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
			exchange_type TEXT NOT NULL,
			testnet INTEGER NOT NULL DEFAULT 0,
			api_key TEXT NOT NULL,
			api_secret TEXT NOT NULL,
			passphrase TEXT NOT NULL DEFAULT '',
			quote_asset TEXT NOT NULL DEFAULT 'USDT',
			hedge_mode INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (exchange_type, testnet)
		)`,
		`CREATE TABLE IF NOT EXISTS active_exchange (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			exchange_type TEXT NOT NULL,
			testnet INTEGER NOT NULL DEFAULT 0,
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
	if err := s.migrateExchangeCredentialsSchema(); err != nil {
		return fmt.Errorf("migrate exchange_credentials schema failed: %w", err)
	}
	if err := s.migrateActiveExchangeSchema(); err != nil {
		return fmt.Errorf("migrate active_exchange schema failed: %w", err)
	}
	return nil
}

// migrateExchangeCredentialsSchema 把旧版本（exchange_type 单列主键，测试网/实盘
// 共用一个槽位）的 exchange_credentials 表迁移到新版本（(exchange_type, testnet)
// 复合主键，测试网和实盘各自独立保存一份凭证，互不覆盖）。
//
// 对全新数据库（表还不存在，或者已经是新schema）这个函数直接返回，不做任何事；
// 只有检测到旧schema时才会真正执行迁移，迁移过程中原有的凭证数据会被保留
// （旧数据里 testnet 字段是什么值，迁移后就归到对应的 testnet/实盘 槽位）。
func (s *Store) migrateExchangeCredentialsSchema() error {
	var tableSQL string
	err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='exchange_credentials'`).Scan(&tableSQL)
	if err == sql.ErrNoRows {
		return nil // 表不存在（不应该发生，上面的建表语句已经执行过），无需迁移
	}
	if err != nil {
		return err
	}
	if strings.Contains(tableSQL, "PRIMARY KEY (exchange_type, testnet)") {
		return nil // 已经是新schema
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE exchange_credentials RENAME TO exchange_credentials_old`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE TABLE exchange_credentials (
			exchange_type TEXT NOT NULL,
			testnet INTEGER NOT NULL DEFAULT 0,
			api_key TEXT NOT NULL,
			api_secret TEXT NOT NULL,
			passphrase TEXT NOT NULL DEFAULT '',
			quote_asset TEXT NOT NULL DEFAULT 'USDT',
			hedge_mode INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (exchange_type, testnet)
		)
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO exchange_credentials (exchange_type, testnet, api_key, api_secret, passphrase, quote_asset, hedge_mode, updated_at)
		SELECT exchange_type, testnet, api_key, api_secret, passphrase, quote_asset, hedge_mode, updated_at FROM exchange_credentials_old
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE exchange_credentials_old`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateActiveExchangeSchema 给旧版本的 active_exchange 表补上 testnet 列
// （旧schema没有这一列，新代码需要它来记住"重启后应该恢复到测试网还是实盘"）。
func (s *Store) migrateActiveExchangeSchema() error {
	rows, err := s.db.Query(`PRAGMA table_info(active_exchange)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasTestnetColumn := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "testnet" {
			hasTestnetColumn = true
		}
	}
	if hasTestnetColumn {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE active_exchange ADD COLUMN testnet INTEGER NOT NULL DEFAULT 0`)
	return err
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

// SaveCredential 保存/更新某个交易所的凭证（绑定）。
// 唯一键是 (exchange_type, testnet) 的组合——测试网和实盘各自独立保存一份，
// 绑定测试网凭证不会覆盖已经绑定好的实盘凭证，反之亦然，这样才能做到
// "在控制台里切换测试网/实盘，不用每次重新填 Key"。
func (s *Store) SaveCredential(c ExchangeCredential) error {
	_, err := s.db.Exec(`
		INSERT INTO exchange_credentials (exchange_type, testnet, api_key, api_secret, passphrase, quote_asset, hedge_mode, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(exchange_type, testnet) DO UPDATE SET
			api_key=excluded.api_key, api_secret=excluded.api_secret, passphrase=excluded.passphrase,
			quote_asset=excluded.quote_asset, hedge_mode=excluded.hedge_mode,
			updated_at=excluded.updated_at
	`, c.ExchangeType, boolToInt(c.Testnet), c.APIKey, c.APISecret, c.Passphrase, c.QuoteAsset, boolToInt(c.HedgeMode), time.Now())
	return err
}

// DeleteCredential 删除某个交易所的凭证（解绑），testnet 用于区分删的是测试网还是实盘那一份
func (s *Store) DeleteCredential(exchangeType string, testnet bool) error {
	_, err := s.db.Exec(`DELETE FROM exchange_credentials WHERE exchange_type = ? AND testnet = ?`, exchangeType, boolToInt(testnet))
	return err
}

// GetCredential 读取某个交易所的凭证（区分测试网/实盘）；不存在时返回 (nil, nil)
func (s *Store) GetCredential(exchangeType string, testnet bool) (*ExchangeCredential, error) {
	row := s.db.QueryRow(`SELECT exchange_type, testnet, api_key, api_secret, passphrase, quote_asset, hedge_mode
		FROM exchange_credentials WHERE exchange_type = ? AND testnet = ?`, exchangeType, boolToInt(testnet))
	var c ExchangeCredential
	var tn, hedge int
	err := row.Scan(&c.ExchangeType, &tn, &c.APIKey, &c.APISecret, &c.Passphrase, &c.QuoteAsset, &hedge)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Testnet = tn != 0
	c.HedgeMode = hedge != 0
	return &c, nil
}

// ListCredentials 列出所有已绑定的交易所凭证（同一个交易所类型可能有测试网和实盘两条）
func (s *Store) ListCredentials() ([]ExchangeCredential, error) {
	rows, err := s.db.Query(`SELECT exchange_type, testnet, api_key, api_secret, passphrase, quote_asset, hedge_mode FROM exchange_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExchangeCredential
	for rows.Next() {
		var c ExchangeCredential
		var tn, hedge int
		if err := rows.Scan(&c.ExchangeType, &tn, &c.APIKey, &c.APISecret, &c.Passphrase, &c.QuoteAsset, &hedge); err != nil {
			return nil, err
		}
		c.Testnet = tn != 0
		c.HedgeMode = hedge != 0
		out = append(out, c)
	}
	return out, nil
}

// SetActiveExchange 记录当前生效的交易所标识（含测试网/实盘），重启后据此自动恢复连接
func (s *Store) SetActiveExchange(exchangeType string, testnet bool) error {
	_, err := s.db.Exec(`
		INSERT INTO active_exchange (id, exchange_type, testnet, updated_at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET exchange_type=excluded.exchange_type, testnet=excluded.testnet, updated_at=excluded.updated_at
	`, exchangeType, boolToInt(testnet), time.Now())
	return err
}

// GetActiveExchange 读取当前生效的交易所标识和测试网/实盘标记；未设置时返回空字符串
func (s *Store) GetActiveExchange() (exchangeType string, testnet bool, err error) {
	var tn int
	row := s.db.QueryRow(`SELECT exchange_type, testnet FROM active_exchange WHERE id = 1`)
	err = row.Scan(&exchangeType, &tn)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return exchangeType, tn != 0, nil
}
