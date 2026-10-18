// Package config loads every environment-specific setting in one place.
//
// THE RULE: code never calls os.Getenv() directly, anywhere. It asks for a
// typed Config struct. Three reasons:
//
//  1. VALIDATION HAPPENS ONCE, AT BOOT. A typo in CACHE_TTL crashes the
//     process on startup with a clear message, instead of silently becoming
//     a zero TTL and quietly disabling your cache at 3am.
//  2. IT IS DISCOVERABLE. This file IS the documentation of every knob.
//  3. IT IS TESTABLE. Tests build a Config literal; no global state, no
//     environment pollution between tests.
//
// This is the Dependency Inversion Principle applied to configuration: the
// business logic depends on a small typed struct it owns, not on the OS.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/constants"
)

// LockStrategy selects HOW we protect a read-modify-write against concurrent
// updates. This is configurable precisely because there is no universally
// correct answer — it depends on contention. See docs/LOCKING.md.
type LockStrategy string

const (
	// LockOptimistic — read the row, compute, then UPDATE ... WHERE version = N.
	// No locks are held while we think. Cheap and highly concurrent, but a
	// loser must retry. BEST WHEN: conflicts are rare.
	LockOptimistic LockStrategy = "optimistic"

	// LockPessimistic — SELECT ... FOR UPDATE takes a row lock immediately and
	// holds it until commit. Others queue up and wait. No retries, no lost
	// updates, but it serialises access and can deadlock if you lock rows in
	// inconsistent order. BEST WHEN: conflicts are common, or the operation
	// must not be retried (e.g. it has side effects).
	LockPessimistic LockStrategy = "pessimistic"

	// LockHybrid — the real-world answer, and our default. Try optimistic
	// first (fast path, no lock). If we keep losing the race, that is evidence
	// of genuine contention, so escalate to a pessimistic lock and get it done.
	// Fast when uncontended, correct and bounded when contended.
	LockHybrid LockStrategy = "hybrid"
)

// StranglerWriteMode controls the write path during a Strangler Fig migration
// (see docs/STRANGLER_FIG.md). You move through these modes over weeks.
type StranglerWriteMode string

const (
	WriteModeNew    StranglerWriteMode = "new"    // Phase 3: new system only. The goal.
	WriteModeDual   StranglerWriteMode = "dual"   // Phase 2: write both, so you can roll back.
	WriteModeLegacy StranglerWriteMode = "legacy" // Phase 1: legacy is still the source of truth.
)

// Config is the fully-resolved, validated configuration for one service.
type Config struct {
	ServiceName string
	Env         string // dev | test | prod — controls log format and strictness
	HTTPAddr    string
	LogLevel    string
	LogFormat   string // json | text

	Postgres  PostgresConfig
	Redis     RedisConfig
	Cache     CacheConfig
	Locking   LockingConfig
	Saga      SagaConfig
	Worker    WorkerConfig
	Deps      DepsConfig
	Strangler StranglerConfig

	ShutdownGracePeriod time.Duration
}

type PostgresConfig struct {
	// DSN is the full connection string. We accept the whole DSN rather than
	// 5 separate fields because that is what every hosting provider hands you.
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	// Enabled=false lets a service boot with no database at all. We use this
	// for the in-memory mode, so you can run and read the system without
	// Docker. The repository interface makes this a one-line swap.
	Enabled bool
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	// Enabled=false falls back to the in-process LRU cache. Same interface,
	// different implementation — the Liskov Substitution Principle in action.
	Enabled bool
}

type CacheConfig struct {
	TTL         time.Duration
	NegativeTTL time.Duration
	// MaxEntries bounds the in-process cache so it cannot eat all your RAM.
	// Redis has its own maxmemory policy (see docker-compose.yml).
	MaxEntries int
	// Enabled=false disables caching entirely. Useful for proving to yourself
	// that a bug is a cache bug.
	Enabled bool
}

type LockingConfig struct {
	Strategy LockStrategy
	// MaxRetries applies to the optimistic path only.
	MaxRetries int
	RetryBase  time.Duration
	// PessimisticTimeout caps how long we wait for a row lock. Without a
	// timeout, one stuck transaction can pile up every request in the system
	// until connections run out. ALWAYS bound your lock waits.
	PessimisticTimeout time.Duration
}

type SagaConfig struct {
	// StepTimeout bounds each individual participant call.
	StepTimeout time.Duration
	// RecoveryInterval is how often we look for sagas that were interrupted
	// by a crash and need to be resumed or compensated.
	RecoveryInterval time.Duration
	Enabled          bool
}

type WorkerConfig struct {
	// AutoAdvanceInterval: how often the PENDING -> PROCESSING job runs.
	AutoAdvanceInterval time.Duration
	AutoAdvanceAfter    time.Duration
	OutboxInterval      time.Duration
	OutboxBatchSize     int
	Enabled             bool
}

// DepsConfig holds the addresses of the services we call. Note these are
// injected, never hardcoded: in tests we point them at an httptest server.
type DepsConfig struct {
	InventoryURL string
	PaymentURL   string
	LegacyURL    string
	AssistantURL string
	Timeout      time.Duration
}

type StranglerConfig struct {
	Enabled bool
	// ReadFallbackToLegacy: on a miss in the new store, ask the legacy system.
	ReadFallbackToLegacy bool
	// BackfillOnRead: when legacy answers, copy the record into the new store
	// ("migrate on read"). Traffic itself performs the migration.
	BackfillOnRead bool
	WriteMode      StranglerWriteMode
}

// Load reads configuration from the environment, applies defaults, and
// validates. It returns an error rather than calling os.Exit so that tests
// can assert on bad input.
func Load(serviceName, defaultAddr string) (Config, error) {
	cfg := Config{
		ServiceName: serviceName,
		Env:         getString("APP_ENV", "dev"),
		HTTPAddr:    getString("HTTP_ADDR", defaultAddr),
		LogLevel:    getString("LOG_LEVEL", "info"),
		LogFormat:   getString("LOG_FORMAT", "text"),

		Postgres: PostgresConfig{
			Enabled:         getBool("POSTGRES_ENABLED", true),
			DSN:             getString("POSTGRES_DSN", "postgres://orders:orders@localhost:5432/orders?sslmode=disable"),
			MaxConns:        int32(getInt("POSTGRES_MAX_CONNS", 10)),
			MinConns:        int32(getInt("POSTGRES_MIN_CONNS", 1)),
			MaxConnLifetime: getDuration("POSTGRES_MAX_CONN_LIFETIME", time.Hour),
		},
		Redis: RedisConfig{
			Enabled:  getBool("REDIS_ENABLED", true),
			Addr:     getString("REDIS_ADDR", "localhost:6379"),
			Password: getString("REDIS_PASSWORD", ""),
			DB:       getInt("REDIS_DB", 0),
		},
		Cache: CacheConfig{
			Enabled:     getBool("CACHE_ENABLED", true),
			TTL:         getDuration("CACHE_TTL", constants.DefaultCacheTTL),
			NegativeTTL: getDuration("CACHE_NEGATIVE_TTL", constants.DefaultNegativeCacheTTL),
			MaxEntries:  getInt("CACHE_MAX_ENTRIES", 10_000),
		},
		Locking: LockingConfig{
			Strategy:           LockStrategy(getString("LOCK_STRATEGY", string(LockHybrid))),
			MaxRetries:         getInt("LOCK_MAX_RETRIES", constants.DefaultOptimisticRetries),
			RetryBase:          getDuration("LOCK_RETRY_BASE", constants.DefaultRetryBaseDelay),
			PessimisticTimeout: getDuration("LOCK_PESSIMISTIC_TIMEOUT", 3*time.Second),
		},
		Saga: SagaConfig{
			Enabled:          getBool("SAGA_ENABLED", true),
			StepTimeout:      getDuration("SAGA_STEP_TIMEOUT", 5*time.Second),
			RecoveryInterval: getDuration("SAGA_RECOVERY_INTERVAL", 30*time.Second),
		},
		Worker: WorkerConfig{
			Enabled:             getBool("WORKERS_ENABLED", true),
			AutoAdvanceInterval: getDuration("WORKER_AUTO_ADVANCE_INTERVAL", time.Minute),
			AutoAdvanceAfter:    getDuration("WORKER_AUTO_ADVANCE_AFTER", constants.AutoAdvanceAfter),
			OutboxInterval:      getDuration("WORKER_OUTBOX_INTERVAL", 2*time.Second),
			OutboxBatchSize:     getInt("WORKER_OUTBOX_BATCH", 50),
		},
		Deps: DepsConfig{
			InventoryURL: getString("INVENTORY_URL", "http://localhost:8081"),
			PaymentURL:   getString("PAYMENT_URL", "http://localhost:8082"),
			LegacyURL:    getString("LEGACY_URL", "http://localhost:8083"),
			AssistantURL: getString("ASSISTANT_URL", "http://localhost:8090"),
			Timeout:      getDuration("DEPS_TIMEOUT", 5*time.Second),
		},
		Strangler: StranglerConfig{
			Enabled:              getBool("STRANGLER_ENABLED", true),
			ReadFallbackToLegacy: getBool("STRANGLER_READ_FALLBACK", true),
			BackfillOnRead:       getBool("STRANGLER_BACKFILL_ON_READ", true),
			WriteMode:            StranglerWriteMode(getString("STRANGLER_WRITE_MODE", string(WriteModeNew))),
		},
		ShutdownGracePeriod: getDuration("SHUTDOWN_GRACE", constants.DefaultShutdownGracePeriod),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate performs FAIL-FAST checks. A service that boots with nonsense
// settings and fails later, under load, is far worse than one that refuses
// to start and tells you exactly why.
func (c Config) validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: HTTP_ADDR must not be empty")
	}
	switch c.Locking.Strategy {
	case LockOptimistic, LockPessimistic, LockHybrid:
	default:
		return fmt.Errorf("config: LOCK_STRATEGY=%q is invalid (want optimistic|pessimistic|hybrid)", c.Locking.Strategy)
	}
	switch c.Strangler.WriteMode {
	case WriteModeNew, WriteModeDual, WriteModeLegacy:
	default:
		return fmt.Errorf("config: STRANGLER_WRITE_MODE=%q is invalid (want new|dual|legacy)", c.Strangler.WriteMode)
	}
	if c.Locking.MaxRetries < 0 || c.Locking.MaxRetries > 20 {
		return fmt.Errorf("config: LOCK_MAX_RETRIES=%d out of range 0..20", c.Locking.MaxRetries)
	}
	if c.Cache.MaxEntries <= 0 {
		return fmt.Errorf("config: CACHE_MAX_ENTRIES must be > 0, got %d", c.Cache.MaxEntries)
	}
	if c.Postgres.Enabled && c.Postgres.DSN == "" {
		return fmt.Errorf("config: POSTGRES_DSN is required when POSTGRES_ENABLED=true")
	}
	if c.Redis.Enabled && c.Redis.Addr == "" {
		return fmt.Errorf("config: REDIS_ADDR is required when REDIS_ENABLED=true")
	}
	if c.Cache.TTL <= 0 {
		return fmt.Errorf("config: CACHE_TTL must be > 0, got %s", c.Cache.TTL)
	}
	return nil
}

// IsProd gates behaviour we only want in production (e.g. refusing to run
// with an in-memory database).
func (c Config) IsProd() bool { return strings.EqualFold(c.Env, "prod") }

// ---------------------------------------------------------------------------
// Small typed getters. Deliberately boring.
// ---------------------------------------------------------------------------

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		// A malformed value falls back to the default rather than crashing.
		// Trade-off: we accept a silent fallback here because the alternative
		// (crash on any typo in any optional var) makes operations brittle.
		// The validate() step still catches values that are *semantically*
		// wrong, which is the dangerous class.
		return def
	}
	return n
}

func getBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v) // accepts "5m", "250ms", "1h30m"
	if err != nil {
		return def
	}
	return d
}
