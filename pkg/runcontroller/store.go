package runcontroller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runRow 与 run 表的列一一对应。
type runRow struct {
	RunID               string
	State               State
	Substate            Substate
	ActorName           string
	NextInboxSeq        int64
	DeliveredThrough    int64
	IdleSince           *time.Time
	ProvisionReturnedAt *time.Time
	FailureReason       string
	CreatedAt           time.Time
	EndedAt             *time.Time
}

// inboxMessage 是 inbox 表中的一行。
type inboxMessage struct {
	Seq       int64
	Text      string
	CreatedAt time.Time
}

// suspensionRow 与 suspension 表的列一一对应。
type suspensionRow struct {
	Seq               int64
	StartedAt         time.Time
	SnapshotMS        *int64
	SnapshotBytes     *int64
	ResumeRequestedAt *time.Time
	RestoreMS         *int64
	RestoreReturnedAt *time.Time
	HelloAt           *time.Time
	HelloMS           *int64
	Error             string
}

// suspensionUpdate 是 updateLatestSuspension 要写入的字段。nil 表示该列保持原值。
type suspensionUpdate struct {
	SnapshotMS        *int64
	SnapshotBytes     *int64
	ResumeRequestedAt *time.Time
	RestoreMS         *int64
	RestoreReturnedAt *time.Time
	HelloAt           *time.Time
	HelloMS           *int64
	Error             *string
}

const runSelectColumns = `run_id, state, substate, actor_name, next_inbox_seq, ` +
	`delivered_through, idle_since, provision_returned_at, failure_reason, created_at, ended_at`

const suspensionSelectColumns = `seq, started_at, snapshot_ms, snapshot_bytes, ` +
	`resume_requested_at, restore_ms, restore_returned_at, hello_at, hello_ms, error`

// store 是 runcontroller 的存储层。事务之外的只读查询直接使用连接池。
type store struct {
	pool *pgxpool.Pool

	// 以下两个钩子只由测试经 export_test.go 安装。
	//
	// commitErrorAfterSuccess 在 Commit 成功之后调用；返回非 nil 时 inTx 把
	// 它当作提交错误返回，用来模拟“提交实际成功却返回错误”。
	commitErrorAfterSuccess func() error
	// afterCommitError 在 Commit 返回错误之后、调用方判定结果之前调用，
	// 用来在报错与判定之间插入另一个事务。
	afterCommitError func()
	// commitStatusError 在设置后替代 pg_xact_status 查询并返回错误，用来让
	// 判定结果的重试全部失败。
	commitStatusError func() error
	hookMu            sync.Mutex
}

func (s *store) setCommitErrorAfterSuccess(fn func() error) {
	s.hookMu.Lock()
	s.commitErrorAfterSuccess = fn
	s.hookMu.Unlock()
}

func (s *store) commitErrorHook() func() error {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.commitErrorAfterSuccess
}

func (s *store) setAfterCommitError(fn func()) {
	s.hookMu.Lock()
	s.afterCommitError = fn
	s.hookMu.Unlock()
}

func (s *store) afterCommitErrorHook() func() {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.afterCommitError
}

func (s *store) setCommitStatusError(fn func() error) {
	s.hookMu.Lock()
	s.commitStatusError = fn
	s.hookMu.Unlock()
}

func (s *store) commitStatusErrorHook() func() error {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.commitStatusError
}

func newStore(pool *pgxpool.Pool) *store {
	return &store{pool: pool}
}

var (
	errRunNotLocked = errors.New("runcontroller: run row not locked by this transaction")
	errNoSuspension = errors.New("runcontroller: run has no suspension record")
)

// storeTx 只使用 inTx 打开的这一个事务连接。
type storeTx struct {
	ctx    context.Context
	tx     pgx.Tx
	locked map[string]struct{} // 本事务已经用 lockRun 锁定的运行实例
}

// commitError 标记 fn 成功但 Commit 返回错误。此时事务是否已提交不确定，
// 调用方要用新连接按 txid 查询事务状态判定。
type commitError struct {
	txid string
	err  error
}

func (e *commitError) Error() string { return e.err.Error() }
func (e *commitError) Unwrap() error { return e.err }

// commitOutcome 是 pg_xact_status 给出的提交结果。
type commitOutcome int

const (
	// commitUnknown 表示重试用尽后仍无法判定。
	commitUnknown commitOutcome = iota
	commitAborted
	commitCommitted
)

// 提交结果的查询上限：最多 10 次，间隔从 50 ms 起翻倍、上限 1 s，总计约
// 5.5 s。超过上限仍为 in progress 或查询失败时按 commitUnknown 处理。
const (
	commitStatusAttempts     = 10
	commitStatusInitialDelay = 50 * time.Millisecond
	commitStatusMaxDelay     = 1 * time.Second
)

// txTimeout 是一次规则事务的上限。事务使用 context.WithoutCancel 开始的 ctx，
// 调用方取消不影响已开始的事务，超时由这个上限兜底。
const txTimeout = 30 * time.Second

// inTx 在一个 READ COMMITTED 事务中执行 fn。fn 返回错误则回滚并返回该错误，
// 否则提交并返回提交的错误；提交错误用 commitError 包装，以区别于 fn 的
// 错误。调用方 ctx 只决定是否开始事务：开始前已取消则直接返回，开始后
// 用脱离取消的 ctx 加 txTimeout 上限执行。
func (s *store) inTx(ctx context.Context, fn func(tx *storeTx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txTimeout)
	defer cancel()

	tx, err := s.pool.BeginTx(txCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Record the transaction ID before anything else. It is the only reliable
	// way to decide the outcome when Commit itself returns an error: neither
	// the connection nor any row version can tell a commit apart from a
	// rollback after another transaction has changed the row.
	var txid string
	if err := tx.QueryRow(txCtx, `SELECT pg_current_xact_id()::text`).Scan(&txid); err != nil {
		return fmt.Errorf("runcontroller: read transaction id: %w", err)
	}

	if err := fn(&storeTx{ctx: txCtx, tx: tx, locked: make(map[string]struct{})}); err != nil {
		return err
	}
	commitErr := tx.Commit(txCtx)
	if commitErr == nil {
		// A test hook may pretend the commit failed although it succeeded.
		if hook := s.commitErrorHook(); hook != nil {
			commitErr = hook()
		}
	}
	if commitErr != nil {
		if hook := s.afterCommitErrorHook(); hook != nil {
			hook()
		}
		return &commitError{txid: txid, err: commitErr}
	}
	return nil
}

// commitOutcome asks PostgreSQL whether the transaction with ce.txid
// committed. "in progress" and query failures are retried a bounded number of
// times; when the retries run out the outcome is commitUnknown.
func (s *store) commitOutcome(ce *commitError) commitOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	delay := commitStatusInitialDelay
	for attempt := 0; attempt < commitStatusAttempts; attempt++ {
		var status *string
		var err error
		if hook := s.commitStatusErrorHook(); hook != nil {
			err = hook()
		} else {
			err = s.pool.QueryRow(ctx, `SELECT pg_xact_status($1::xid8)`, ce.txid).Scan(&status)
		}
		if err == nil && status != nil {
			switch *status {
			case "committed":
				return commitCommitted
			case "aborted":
				return commitAborted
			}
		}
		if attempt == commitStatusAttempts-1 {
			break
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return commitUnknown
		}
		if delay < commitStatusMaxDelay {
			delay *= 2
			if delay > commitStatusMaxDelay {
				delay = commitStatusMaxDelay
			}
		}
	}
	return commitUnknown
}

// requireLock 确认本事务已经锁定了该运行实例。会读取已有行内容再决定如何
// 写入的方法必须先 lockRun，否则两个事务可能基于同一份旧状态互相覆盖。
func (t *storeTx) requireLock(runID string) error {
	if _, ok := t.locked[runID]; !ok {
		return fmt.Errorf("%w: %s", errRunNotLocked, runID)
	}
	return nil
}

// insertRun 写入运行实例与收件箱第 1 条消息。
func (t *storeTx) insertRun(r runRow, firstText string, now time.Time) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO run (
		run_id, state, substate, actor_name, next_inbox_seq, delivered_through,
		idle_since, provision_returned_at, failure_reason, created_at, ended_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.RunID, r.State, r.Substate, r.ActorName, r.NextInboxSeq, r.DeliveredThrough,
		r.IdleSince, r.ProvisionReturnedAt, r.FailureReason, r.CreatedAt, r.EndedAt,
	)
	if err != nil {
		return fmt.Errorf("runcontroller: insert run %s: %w", r.RunID, err)
	}
	if err := t.insertInbox(r.RunID, inboxInsert{Seq: 1, Kind: KindHumanMessage, Text: firstText}, now); err != nil {
		return err
	}
	return nil
}

// lockRun 以 SELECT ... FOR UPDATE 读取运行实例，不存在返回 ErrNotFound。
func (t *storeTx) lockRun(runID string) (runRow, error) {
	r, err := scanRunRow(t.tx.QueryRow(t.ctx,
		`SELECT `+runSelectColumns+` FROM run WHERE run_id = $1 FOR UPDATE`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return runRow{}, ErrNotFound
	}
	if err != nil {
		return runRow{}, fmt.Errorf("runcontroller: lock run %s: %w", runID, err)
	}
	if t.locked == nil {
		t.locked = make(map[string]struct{})
	}
	t.locked[runID] = struct{}{}
	return r, nil
}

// updateRun 写回 runRow 的全部可变列。调用前必须已用 lockRun 锁定该行；
// 未锁先写返回 errRunNotLocked。行不存在时 updateRun 返回 ErrNotFound，但
// 正常路径上 lockRun 已经先把它挡掉。
//
// 提交结果只按事务状态判定，不再依赖 version 列；该列保留只为兼容已有
// 数据库，仍然递增但没有任何读取方。
func (t *storeTx) updateRun(r runRow) error {
	if err := t.requireLock(r.RunID); err != nil {
		return err
	}
	tag, err := t.tx.Exec(t.ctx, `UPDATE run SET
		state = $2, substate = $3, actor_name = $4, next_inbox_seq = $5,
		delivered_through = $6, idle_since = $7, provision_returned_at = $8,
		failure_reason = $9, ended_at = $10, version = version + 1
	WHERE run_id = $1`,
		r.RunID, r.State, r.Substate, r.ActorName, r.NextInboxSeq,
		r.DeliveredThrough, r.IdleSince, r.ProvisionReturnedAt,
		r.FailureReason, r.EndedAt,
	)
	if err != nil {
		return fmt.Errorf("runcontroller: update run %s: %w", r.RunID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// inboxInsert 是 insertInbox 要写入的一行。Payload 只有
// subscription_completed 事件才有，按 JSON 存入 jsonb 列 payload。
type inboxInsert struct {
	Seq      int64
	Kind     InboxKind
	Text     string
	EventID  string
	DedupKey string
	Payload  []byte
}

// insertInbox 写入一条收件箱消息。它只新增行，不读取已有行的内容，
// 序号由调用方给出，因此不要求先 lockRun。
func (t *storeTx) insertInbox(runID string, m inboxInsert, now time.Time) error {
	// A nil interface becomes SQL NULL; a non-nil string is encoded as jsonb
	// raw JSON by pgx's JSON codec.
	var payload any
	if len(m.Payload) > 0 {
		payload = string(m.Payload)
	}
	_, err := t.tx.Exec(t.ctx,
		`INSERT INTO inbox (run_id, inbox_seq, kind, text, event_id, dedup_key, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		runID, m.Seq, string(m.Kind), m.Text, m.EventID, m.DedupKey, payload, now)
	if err != nil {
		return fmt.Errorf("runcontroller: insert inbox %s/%d: %w", runID, m.Seq, err)
	}
	return nil
}

// inboxSeqByDedupKey 返回该运行实例中 dedup_key 匹配的收件箱行的序号。
// 没有匹配时 found 为 false。调用方必须已用 lockRun 锁定运行实例行。
func (t *storeTx) inboxSeqByDedupKey(runID, dedupKey string) (seq int64, found bool, err error) {
	if err := t.requireLock(runID); err != nil {
		return 0, false, err
	}
	err = t.tx.QueryRow(t.ctx,
		`SELECT inbox_seq FROM inbox WHERE run_id = $1 AND dedup_key = $2`,
		runID, dedupKey).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("runcontroller: look up inbox dedup key %s/%s: %w", runID, dedupKey, err)
	}
	return seq, true, nil
}

// inboxRange 按 inbox_seq 升序取 [from, through] 内的消息。累计文本字节数超过
// maxBytes 前停止，但至少返回一条。
func (t *storeTx) inboxRange(runID string, from, through int64, maxBytes int) ([]inboxMessage, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT inbox_seq, text, created_at
		FROM inbox
		WHERE run_id = $1 AND inbox_seq >= $2 AND inbox_seq <= $3
		ORDER BY inbox_seq ASC`, runID, from, through)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: read inbox %s: %w", runID, err)
	}
	defer rows.Close()

	messages := make([]inboxMessage, 0)
	total := 0
	for rows.Next() {
		var m inboxMessage
		if err := rows.Scan(&m.Seq, &m.Text, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("runcontroller: scan inbox %s: %w", runID, err)
		}
		if len(messages) > 0 && total+len(m.Text) > maxBytes {
			break
		}
		total += len(m.Text)
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runcontroller: read inbox %s: %w", runID, err)
	}
	return messages, nil
}

// upsertTurnStarted 记录回合开始；回合已存在时不变。它只新增行，
// 冲突时不改动已有内容，因此不要求先 lockRun。
func (t *storeTx) upsertTurnStarted(runID, turnID, deliveryID string, at time.Time) error {
	_, err := t.tx.Exec(t.ctx, `INSERT INTO turn (run_id, turn_id, delivery_id, started_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, turn_id) DO NOTHING`,
		runID, turnID, deliveryID, at)
	if err != nil {
		return fmt.Errorf("runcontroller: upsert turn start %s/%s: %w", runID, turnID, err)
	}
	return nil
}

// upsertTurnEnded 记录回合结束：回合不存在时以 started_at=at 新建，已有结束时间时不变。
func (t *storeTx) upsertTurnEnded(runID, turnID string, at time.Time, output string, isError bool) error {
	if err := t.requireLock(runID); err != nil {
		return err
	}
	_, err := t.tx.Exec(t.ctx, `INSERT INTO turn (
		run_id, turn_id, delivery_id, started_at, ended_at, output, is_error
	) VALUES ($1, $2, '', $3, $3, $4, $5)
	ON CONFLICT (run_id, turn_id) DO UPDATE SET
		ended_at = EXCLUDED.ended_at,
		output = EXCLUDED.output,
		is_error = EXCLUDED.is_error
	WHERE turn.ended_at IS NULL`,
		runID, turnID, at, output, isError)
	if err != nil {
		return fmt.Errorf("runcontroller: upsert turn end %s/%s: %w", runID, turnID, err)
	}
	return nil
}

// openSuspension 为运行实例新增一行挂起记录，seq 从 1 起递增。
func (t *storeTx) openSuspension(runID string, at time.Time) error {
	if err := t.requireLock(runID); err != nil {
		return err
	}
	_, err := t.tx.Exec(t.ctx, `INSERT INTO suspension (run_id, seq, started_at)
		SELECT $1, COALESCE(MAX(seq), 0) + 1, $2
		FROM suspension WHERE run_id = $1`, runID, at)
	if err != nil {
		return fmt.Errorf("runcontroller: open suspension for %s: %w", runID, err)
	}
	return nil
}

// latestSuspension 返回最近一次挂起记录；没有记录时 ok 为 false。
func (t *storeTx) latestSuspension(runID string) (suspensionRow, bool, error) {
	row, err := scanSuspensionRow(t.tx.QueryRow(t.ctx,
		`SELECT `+suspensionSelectColumns+`
		FROM suspension WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return suspensionRow{}, false, nil
	}
	if err != nil {
		return suspensionRow{}, false, fmt.Errorf("runcontroller: read latest suspension for %s: %w", runID, err)
	}
	return row, true, nil
}

// updateLatestSuspension 只写入 u 中给出的字段，其余列保持原值。
// 没有挂起记录时报错，而不是静默地更新零行。
func (t *storeTx) updateLatestSuspension(runID string, u suspensionUpdate) error {
	if err := t.requireLock(runID); err != nil {
		return err
	}
	tag, err := t.tx.Exec(t.ctx, `UPDATE suspension SET
		snapshot_ms = COALESCE($2::bigint, snapshot_ms),
		snapshot_bytes = COALESCE($3::bigint, snapshot_bytes),
		resume_requested_at = COALESCE($4::timestamptz, resume_requested_at),
		restore_ms = COALESCE($5::bigint, restore_ms),
		restore_returned_at = COALESCE($6::timestamptz, restore_returned_at),
		hello_at = COALESCE($7::timestamptz, hello_at),
		hello_ms = COALESCE($8::bigint, hello_ms),
		error = COALESCE($9::text, error)
	WHERE run_id = $1 AND seq = (
		SELECT MAX(seq) FROM suspension WHERE run_id = $1
	)`,
		runID, u.SnapshotMS, u.SnapshotBytes, u.ResumeRequestedAt, u.RestoreMS,
		u.RestoreReturnedAt, u.HelloAt, u.HelloMS, u.Error)
	if err != nil {
		return fmt.Errorf("runcontroller: update suspension for %s: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", errNoSuspension, runID)
	}
	return nil
}

// getRun 读取运行实例的完整详情。不存在返回 ErrNotFound。
func (s *store) getRun(ctx context.Context, runID string) (RunDetail, error) {
	r, err := scanRunRow(s.pool.QueryRow(ctx,
		`SELECT `+runSelectColumns+` FROM run WHERE run_id = $1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunDetail{}, ErrNotFound
	}
	if err != nil {
		return RunDetail{}, fmt.Errorf("runcontroller: get run %s: %w", runID, err)
	}

	turns, err := s.turns(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	suspensions, err := s.suspensions(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}

	return RunDetail{
		RunSummary: RunSummary{
			RunID:     r.RunID,
			State:     r.State,
			Substate:  r.Substate,
			CreatedAt: r.CreatedAt,
		},
		FailureReason: r.FailureReason,
		Turns:         turns,
		Suspensions:   suspensions,
	}, nil
}

func (s *store) turns(ctx context.Context, runID string) ([]Turn, error) {
	rows, err := s.pool.Query(ctx, `SELECT turn_id, started_at, ended_at, output, is_error
		FROM turn WHERE run_id = $1 ORDER BY started_at ASC, turn_id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: list turns for %s: %w", runID, err)
	}
	defer rows.Close()

	turns := make([]Turn, 0)
	for rows.Next() {
		var turn Turn
		if err := rows.Scan(&turn.TurnID, &turn.StartedAt, &turn.EndedAt, &turn.Output, &turn.IsError); err != nil {
			return nil, fmt.Errorf("runcontroller: scan turn for %s: %w", runID, err)
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runcontroller: list turns for %s: %w", runID, err)
	}
	return turns, nil
}

func (s *store) suspensions(ctx context.Context, runID string) ([]Suspension, error) {
	rows, err := s.pool.Query(ctx, `SELECT started_at, snapshot_ms, restore_ms, hello_ms, error
		FROM suspension WHERE run_id = $1 ORDER BY started_at ASC, seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: list suspensions for %s: %w", runID, err)
	}
	defer rows.Close()

	suspensions := make([]Suspension, 0)
	for rows.Next() {
		var suspension Suspension
		if err := rows.Scan(&suspension.StartedAt, &suspension.SnapshotMS, &suspension.RestoreMS,
			&suspension.HelloMS, &suspension.Error); err != nil {
			return nil, fmt.Errorf("runcontroller: scan suspension for %s: %w", runID, err)
		}
		suspensions = append(suspensions, suspension)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runcontroller: list suspensions for %s: %w", runID, err)
	}
	return suspensions, nil
}

// listRuns 按 created_at 降序列出全部运行实例，相同时按 run_id 降序。
func (s *store) listRuns(ctx context.Context) ([]RunSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id, state, substate, created_at
		FROM run ORDER BY created_at DESC, run_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: list runs: %w", err)
	}
	defer rows.Close()

	runs := make([]RunSummary, 0)
	for rows.Next() {
		var summary RunSummary
		if err := rows.Scan(&summary.RunID, &summary.State, &summary.Substate, &summary.CreatedAt); err != nil {
			return nil, fmt.Errorf("runcontroller: scan run: %w", err)
		}
		runs = append(runs, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runcontroller: list runs: %w", err)
	}
	return runs, nil
}

// suspendCandidates 返回 active/boundary 且 idle_since 早于 idleBefore 的运行实例。
func (s *store) suspendCandidates(ctx context.Context, idleBefore time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id FROM run
		WHERE state = 'active' AND substate = 'boundary' AND idle_since < $1
		ORDER BY run_id ASC`, idleBefore)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: list suspend candidates: %w", err)
	}
	defer rows.Close()

	return scanRunIDs(rows, "suspend candidates")
}

// helloTimeoutCandidates 返回等待 Hello 已超时的运行实例：resuming 且最近一次
// 挂起记录的 restore_returned_at 早于 returnedBefore、hello_at 为空，或
// provisioning 且 provision_returned_at 早于 returnedBefore。
func (s *store) helloTimeoutCandidates(ctx context.Context, returnedBefore time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.run_id
		FROM run r
		LEFT JOIN LATERAL (
			SELECT restore_returned_at, hello_at
			FROM suspension s
			WHERE s.run_id = r.run_id
			ORDER BY s.seq DESC
			LIMIT 1
		) s ON true
		WHERE (r.state = 'provisioning'
				AND r.provision_returned_at IS NOT NULL
				AND r.provision_returned_at < $1)
			OR (r.state = 'resuming'
				AND s.restore_returned_at IS NOT NULL
				AND s.restore_returned_at < $1
				AND s.hello_at IS NULL)
		ORDER BY r.run_id ASC`, returnedBefore)
	if err != nil {
		return nil, fmt.Errorf("runcontroller: list hello timeout candidates: %w", err)
	}
	defer rows.Close()

	return scanRunIDs(rows, "hello timeout candidates")
}

func scanRunIDs(rows pgx.Rows, what string) ([]string, error) {
	runIDs := make([]string, 0)
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("runcontroller: scan %s: %w", what, err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runcontroller: list %s: %w", what, err)
	}
	return runIDs, nil
}

func scanRunRow(row pgx.Row) (runRow, error) {
	var r runRow
	err := row.Scan(
		&r.RunID, &r.State, &r.Substate, &r.ActorName, &r.NextInboxSeq,
		&r.DeliveredThrough, &r.IdleSince, &r.ProvisionReturnedAt,
		&r.FailureReason, &r.CreatedAt, &r.EndedAt,
	)
	return r, err
}

func scanSuspensionRow(row pgx.Row) (suspensionRow, error) {
	var s suspensionRow
	err := row.Scan(
		&s.Seq, &s.StartedAt, &s.SnapshotMS, &s.SnapshotBytes,
		&s.ResumeRequestedAt, &s.RestoreMS, &s.RestoreReturnedAt,
		&s.HelloAt, &s.HelloMS, &s.Error,
	)
	return s, err
}
