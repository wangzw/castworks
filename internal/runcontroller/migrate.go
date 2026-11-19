package runcontroller

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationAdvisoryLock 是 Migrate 在事务内取的 advisory lock 键。固定值使
// 同一数据库上的并发 Migrate 串行执行。
const migrationAdvisoryLock int64 = 0x72756e636374726c // "runcctrl"

// migration 是一个嵌入包内的迁移文件。
type migration struct {
	version int64
	name    string
	sql     string
}

// Migrate 在 pool 指向的数据库上按版本号应用尚未记录的迁移。
//
// 整个迁移在一个 READ COMMITTED 事务里执行：先取 pg_advisory_xact_lock，
// 再创建 schema_migrations（若不存在），随后按版本号执行未记录的迁移并写入
// 版本行，最后提交。重复执行与并发执行都成功；已有数据的库上再执行不会
// 改动任何数据。New 不执行迁移，控制面每次启动都调用 Migrate。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("runcontroller: begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryLock); err != nil {
		return fmt.Errorf("runcontroller: acquire migration lock: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("runcontroller: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("runcontroller: check migration %s: %w", m.name, err)
		}
		if applied {
			continue
		}

		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("runcontroller: apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name,
		); err != nil {
			return fmt.Errorf("runcontroller: record migration %s: %w", m.name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("runcontroller: commit migrations: %w", err)
	}
	return nil
}

// loadMigrations 读出嵌入的迁移文件并按版本号排序。文件名形如
// 0001_init.sql，下划线前是版本号。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("runcontroller: read migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		base := strings.TrimSuffix(entry.Name(), ".sql")
		sep := strings.IndexByte(base, '_')
		if sep <= 0 {
			return nil, fmt.Errorf("runcontroller: migration %q has no version prefix", entry.Name())
		}
		version, err := strconv.ParseInt(base[:sep], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("runcontroller: migration %q has an invalid version", entry.Name())
		}

		data, err := migrationFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("runcontroller: read migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: entry.Name(), sql: string(data)})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	for i := 1; i < len(migrations); i++ {
		if migrations[i].version == migrations[i-1].version {
			return nil, fmt.Errorf("runcontroller: duplicate migration version %d", migrations[i].version)
		}
	}
	return migrations, nil
}
