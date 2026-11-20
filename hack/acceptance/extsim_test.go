//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// v0.2.0 验收使用的外部系统是 cmd/extsim。它只以子进程方式使用：测试不在
// 进程内 import 它，也不与它共享内存。本文件按 extsim.md、connectorhub.md
// 与 runcontroller.md 的接口把调用写出来，上游实现合入前只保证编译与不依赖
// 真实二进制的框架测试通过。

const (
	// localExtsimJobsAddr 是本地模式与演练中 extsim 作业接口的固定地址。
	// 看守进程只把固定变量交给 Claude Code，开发机上没有 /etc/extsim.json，
	// 所以 Agent 调用的 extsim 命令行只能取默认的 http://127.0.0.1:9100。
	localExtsimJobsAddr = "127.0.0.1:9100"

	extsimWebhookPath = "/hooks/extsim"
	extsimEventType   = "job.completed"

	signingKeySize = 32

	// defaultBGJobsInterval 是 bg_jobs 观察器的读取周期。
	defaultBGJobsInterval = time.Second
)

// ---------------------------------------------------------------------------
// 签名密钥

// newSigningKeyBytes returns 32 random bytes.
func newSigningKeyBytes() ([]byte, error) {
	key := make([]byte, signingKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// writeSigningKeyFile writes key as the signing key file, owner-only.
func writeSigningKeyFile(path string, key []byte) error {
	return os.WriteFile(path, key, 0o600)
}

// generateSigningKey writes a fresh key below the working directory, records
// it for cleanup, and returns its path. Every run has its own key; the shared
// file under deploy/dev is never used.
func (h *harness) generateSigningKey() string {
	h.t.Helper()
	key, err := newSigningKeyBytes()
	if err != nil {
		h.t.Fatalf("acceptance: generate signing key: %v", err)
	}
	path := filepath.Join(h.workDir, "extsim-signing-key")
	if err := writeSigningKeyFile(path, key); err != nil {
		h.t.Fatalf("acceptance: write signing key %s: %v", path, err)
	}
	// Load the key back through the same newline rule the control plane and
	// extsim apply, so a replayed signature uses exactly their key.
	loaded, err := readSigningKey(path)
	if err != nil {
		h.t.Fatalf("acceptance: read signing key %s: %v", path, err)
	}
	h.signingKeyFile = path
	h.signingKey = loaded
	h.recordGeneratedFiles(path)
	return path
}

// readSigningKey reads a signing key file. The content is used as bytes after
// one trailing newline (either "\n" or "\r\n") is removed, which is the rule
// connectorhub follows.
func readSigningKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch {
	case bytes.HasSuffix(data, []byte("\r\n")):
		data = bytes.TrimSuffix(data, []byte("\r\n"))
	case bytes.HasSuffix(data, []byte("\n")):
		data = bytes.TrimSuffix(data, []byte("\n"))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("signing key file %s is empty", path)
	}
	return data, nil
}

// signExtsimBody returns the value of X-Extsim-Signature-256 for body:
// "sha256=" followed by the hex HMAC-SHA256 of the request body.
func signExtsimBody(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// connectorhub 配置

// defaultExtsimDefinition returns the repository's extsim definition file. The
// generated configuration points at it by absolute path.
func defaultExtsimDefinition(root string) string {
	return filepath.Join(root, "deploy", "dev", "connectorhub", "extsim.yaml")
}

// connectorhubConfigYAML renders the per-run connectorhub configuration.
func connectorhubConfigYAML(definition, signingKeyFile, apiBaseURL string) string {
	return fmt.Sprintf(`project: default
systems:
  - definition: %s
    signing_key_file: %s
    api_base_url: %s
bindings:
  extsim: [demo]
`,
		strconv.Quote(definition),
		strconv.Quote(signingKeyFile),
		strconv.Quote(apiBaseURL),
	)
}

// writeConnectorhubConfig writes the per-run connectorhub configuration and
// records it for cleanup. definition may be empty, in which case the file in
// the repository is used.
func (h *harness) writeConnectorhubConfig(definition, signingKeyFile, apiBaseURL string) string {
	h.t.Helper()
	if definition == "" {
		definition = defaultExtsimDefinition(h.repoRoot)
	}
	if !filepath.IsAbs(definition) {
		definition, _ = filepath.Abs(definition)
	}
	path := filepath.Join(h.workDir, "connectorhub.yaml")
	content := connectorhubConfigYAML(definition, signingKeyFile, apiBaseURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatalf("acceptance: write connectorhub config %s: %v", path, err)
	}
	h.recordGeneratedFiles(path)
	return path
}

// ---------------------------------------------------------------------------
// extsim 子进程

// extsimOptions carries the parameters extsim serve takes. Empty fields take
// the values the local acceptance path uses.
type extsimOptions struct {
	// Listen is the jobs interface. Local runs fix it to
	// 127.0.0.1:9100 because the Agent's extsim command line falls back to
	// that address.
	Listen string
	// ControlListen is the loopback-only test control interface.
	ControlListen string
	// CallbackURL defaults to the webhook receiver of the running control
	// plane.
	CallbackURL string
	// SigningKeyFile defaults to the key generated for this run.
	SigningKeyFile string
	JobDuration    time.Duration
}

// requireFreeAddr checks that nothing is listening on addr.
func requireFreeAddr(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return listener.Close()
}

// startExtsim starts the extsim binary after the control plane, so that the
// callback URL can be taken from the control plane's webhook receiver. It
// waits for both listeners and stores their actual addresses.
func (h *harness) startExtsim(ctx context.Context, opts extsimOptions) {
	h.t.Helper()

	if opts.Listen == "" {
		opts.Listen = localExtsimJobsAddr
	}
	if opts.ControlListen == "" {
		opts.ControlListen = "127.0.0.1:0"
	}
	if opts.JobDuration == 0 {
		opts.JobDuration = 60 * time.Second
	}
	if opts.CallbackURL == "" {
		if h.webhookAddr == "" {
			h.t.Fatal("acceptance: start extsim after the control plane with --connectorhub-config; the callback URL comes from the webhook listener")
		}
		opts.CallbackURL = "http://" + h.webhookAddr + extsimWebhookPath
	}
	if opts.SigningKeyFile == "" {
		if h.signingKeyFile == "" {
			h.t.Fatal("acceptance: start extsim with a signing key file")
		}
		opts.SigningKeyFile = h.signingKeyFile
	}
	key, err := readSigningKey(opts.SigningKeyFile)
	if err != nil {
		h.t.Fatalf("acceptance: read signing key %s: %v", opts.SigningKeyFile, err)
	}
	h.signingKeyFile = opts.SigningKeyFile
	h.signingKey = key

	if err := requireFreeAddr(opts.Listen); err != nil {
		h.t.Fatalf("acceptance: extsim jobs address %s is not free: %v; "+
			"a Substrate acceptance run or a connectorhub integration test may be running", opts.Listen, err)
	}

	args := []string{
		"serve",
		"--listen", opts.Listen,
		"--control-listen", opts.ControlListen,
		"--callback-url", opts.CallbackURL,
		"--signing-key-file", opts.SigningKeyFile,
		"--job-duration", opts.JobDuration.String(),
	}
	h.extsim = h.startChild("extsim", filepath.Join(h.binDir, "extsim"), args)

	h.extsimJobsAddr = h.waitLoggedListenerAddr(ctx, "jobs")
	h.extsimControlAddr = h.waitLoggedListenerAddr(ctx, "control")
	h.extsimCtl = &extsimControl{
		baseURL: "http://" + h.extsimControlAddr,
		client:  &http.Client{Timeout: httpTimeout},
	}
}

// loggedListener matches the listener attribute of an extsim log line.
var loggedListener = regexp.MustCompile(`\blistener="?([a-z]+)"?`)

// waitLoggedListenerAddr waits for the extsim "listening" line whose listener
// attribute is name, and returns its addr attribute.
func (h *harness) waitLoggedListenerAddr(ctx context.Context, name string) string {
	h.t.Helper()
	paths := []string{
		filepath.Join(h.workDir, "extsim.stderr.log"),
		filepath.Join(h.workDir, "extsim.stdout.log"),
	}
	listenerAddr, err := waitForListenerAddr(ctx, paths, name)
	if err != nil {
		h.t.Fatalf("acceptance: read extsim %s address: %v", name, err)
	}
	return listenerAddr
}

// waitForListenerAddr polls log files until one holds a listening line whose
// listener attribute equals name, and returns that line's addr.
func waitForListenerAddr(ctx context.Context, paths []string, name string) (string, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		for _, path := range paths {
			if addr := listenerAddrIn(path, name); addr != "" {
				return addr, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for extsim listener %q in %s: %w", name, strings.Join(paths, ", "), ctx.Err())
		case <-ticker.C:
		}
	}
}

// listenerAddrIn returns the addr of the first line in path that reports a
// listening listener named listener, or "" when there is none.
func listenerAddrIn(path, listener string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "listening") {
			continue
		}
		match := loggedListener.FindStringSubmatch(line)
		if len(match) != 2 || match[1] != listener {
			continue
		}
		if addr := loggedAddr.FindStringSubmatch(line); len(addr) == 2 {
			return addr[1]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// extsim 测试控制客户端

// extsimPlan is one entry of POST /control/plans.
type extsimPlan struct {
	DurationMS      int64  `json:"duration_ms,omitempty"`
	Callback        string `json:"callback,omitempty"`
	CallbackDelayMS int64  `json:"callback_delay_ms,omitempty"`
}

// extsimControl talks to the loopback test control interface.
type extsimControl struct {
	baseURL string
	client  *http.Client
}

// extsimControlClient returns the control client of the running extsim.
func (h *harness) extsimControlClient() *extsimControl {
	h.t.Helper()
	if h.extsimCtl == nil {
		h.t.Fatal("acceptance: extsim is not running; start it before using the control interface")
	}
	return h.extsimCtl
}

// appendPlan adds a job plan. Plans are used in the order they were appended.
func (c *extsimControl) appendPlan(ctx context.Context, plan extsimPlan) error {
	return c.do(ctx, http.MethodPost, "/control/plans", plan, nil)
}

// clearPlans discards every plan that has not been used yet.
func (c *extsimControl) clearPlans(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/control/plans", nil, nil)
}

// deliveries returns the delivery attempts of one job.
func (c *extsimControl) deliveries(ctx context.Context, jobID string) ([]extsimDelivery, error) {
	var body json.RawMessage
	path := "/control/jobs/" + jobID + "/deliveries"
	if err := c.do(ctx, http.MethodGet, path, nil, &body); err != nil {
		return nil, err
	}
	return parseExtsimDeliveries(body)
}

func (c *extsimControl) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// extsim 作业接口

// extsimJob is the part of the job object the acceptance runs read.
type extsimJob struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Reason     string    `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// extsimJobsClient talks to the extsim job interface. The real local and
// Substrate runs let the Agent submit; the criteria that depend on the order
// of the callback and the registration have the test submit instead.
type extsimJobsClient struct {
	baseURL string
	client  *http.Client
}

// extsimJobs returns the job client of the running extsim.
func (h *harness) extsimJobs() *extsimJobsClient {
	h.t.Helper()
	if h.extsimJobsAddr == "" {
		h.t.Fatal("acceptance: extsim is not running; start it before submitting jobs")
	}
	return &extsimJobsClient{baseURL: "http://" + h.extsimJobsAddr, client: &http.Client{Timeout: httpTimeout}}
}

// submitJob posts one job and returns the job object.
func (c *extsimJobsClient) submitJob(ctx context.Context, project, content string) (extsimJob, error) {
	var job extsimJob
	err := c.do(ctx, http.MethodPost, "/v1/jobs", map[string]string{"project": project, "content": content}, &job)
	return job, err
}

// getJob reads one job. A missing job is reported as an HTTP status error.
func (c *extsimJobsClient) getJob(ctx context.Context, id string) (extsimJob, error) {
	var job extsimJob
	err := c.do(ctx, http.MethodGet, "/v1/jobs/"+id, nil, &job)
	return job, err
}

func (c *extsimJobsClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// replayWebhook posts body to the control plane's webhook receiver with a
// fresh signature and the given delivery identifier. It serves the duplicate
// delivery case where the same identifier arrives twice: extsim's control
// interface only produces new identifiers.
func (h *harness) replayWebhook(ctx context.Context, deliveryID string, body []byte) (int, error) {
	if h.webhookAddr == "" {
		return 0, fmt.Errorf("the control plane has no webhook receiver")
	}
	if len(h.signingKey) == 0 {
		return 0, fmt.Errorf("no signing key is loaded")
	}
	url := "http://" + h.webhookAddr + extsimWebhookPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Extsim-Event", extsimEventType)
	req.Header.Set("X-Extsim-Delivery", deliveryID)
	req.Header.Set("X-Extsim-Signature-256", signExtsimBody(h.signingKey, body))
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// 数据库读取

// inboxRecord is one row of the inbox table. Payload is the subscription
// completion stored for a subscription_completed row and nil for a human
// message.
type inboxRecord struct {
	RunID     string
	Seq       int64
	Kind      string
	Text      string
	EventID   string
	DedupKey  string
	Payload   []byte
	CreatedAt time.Time
}

// runRecord is the part of the run table the v0.2.0 criteria read.
type runRecord struct {
	RunID     string
	State     string
	Substate  string
	BGJobs    []string
	IdleSince *time.Time
}

// subscriptionRecord is one row of the subscription table.
type subscriptionRecord struct {
	SubID       string
	RunID       string
	ProjectID   string
	System      string
	Event       string
	Status      string
	Terminal    string
	Correlation map[string]string
	SinceCursor int64
	DedupSpec   []byte
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// connectorEventRecord is one row of the connector_event table.
type connectorEventRecord struct {
	AppendSeq   int64
	System      string
	DeliveryID  string
	EventType   string
	ProjectIDs  []string
	Correlation map[string]string
	OccurredAt  time.Time
	ReceivedAt  time.Time
	Payload     []byte
}

// completionDeliveryRecord is one row of the completion_delivery table.
type completionDeliveryRecord struct {
	SubID       string
	RunID       string
	EventID     string
	DedupKey    string
	Event       []byte
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

// openRunDatabase opens the control plane's database. Callers close the pool.
func (h *harness) openRunDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	if h.database == "" {
		return nil, fmt.Errorf("the harness has no database URL")
	}
	return pgxpool.New(ctx, h.database)
}

func decodeJSONMap(data []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var value map[string]string
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// readInbox returns the inbox rows of a run in inbox_seq order.
func (h *harness) readInbox(ctx context.Context, runID string) ([]inboxRecord, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT run_id, inbox_seq, kind, text, event_id, dedup_key, payload, created_at
		   FROM inbox WHERE run_id = $1 ORDER BY inbox_seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []inboxRecord
	for rows.Next() {
		var (
			record   inboxRecord
			kind     sql.NullString
			eventID  sql.NullString
			dedupKey sql.NullString
		)
		if err := rows.Scan(&record.RunID, &record.Seq, &kind, &record.Text,
			&eventID, &dedupKey, &record.Payload, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.Kind = kind.String
		record.EventID = eventID.String
		record.DedupKey = dedupKey.String
		records = append(records, record)
	}
	return records, rows.Err()
}

// readTurns returns the turns of a run in start order, with the delivery
// identifier of the inbox batch that started each turn. GET /v1/runs does not
// expose the delivery identifier, which the turn/inbox correspondence needs.
func (h *harness) readTurns(ctx context.Context, runID string) ([]turn, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT turn_id, delivery_id, started_at, ended_at, output, is_error
		   FROM turn WHERE run_id = $1 ORDER BY started_at, turn_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []turn
	for rows.Next() {
		var record turn
		if err := rows.Scan(&record.TurnID, &record.DeliveryID, &record.StartedAt,
			&record.EndedAt, &record.Output, &record.IsError); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// readRun returns the run row, including the background job set.
func (h *harness) readRun(ctx context.Context, runID string) (runRecord, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return runRecord{}, err
	}
	defer pool.Close()

	var record runRecord
	err = pool.QueryRow(ctx,
		`SELECT run_id, state, substate, bg_jobs, idle_since FROM run WHERE run_id = $1`, runID).
		Scan(&record.RunID, &record.State, &record.Substate, &record.BGJobs, &record.IdleSince)
	return record, err
}

// readSubscriptions returns the subscriptions of a run in creation order.
func (h *harness) readSubscriptions(ctx context.Context, runID string) ([]subscriptionRecord, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT sub_id, run_id, project_id, system, event, status, terminal,
		        correlation, since_cursor, dedup_spec, created_at, completed_at
		   FROM subscription WHERE run_id = $1 ORDER BY created_at, sub_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []subscriptionRecord
	for rows.Next() {
		var (
			record      subscriptionRecord
			terminal    sql.NullString
			correlation []byte
			dedupSpec   []byte
		)
		if err := rows.Scan(&record.SubID, &record.RunID, &record.ProjectID, &record.System,
			&record.Event, &record.Status, &terminal, &correlation, &record.SinceCursor,
			&dedupSpec, &record.CreatedAt, &record.CompletedAt); err != nil {
			return nil, err
		}
		record.Terminal = terminal.String
		record.DedupSpec = dedupSpec
		if record.Correlation, err = decodeJSONMap(correlation); err != nil {
			return nil, fmt.Errorf("subscription %s correlation: %w", record.SubID, err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// readConnectorEvents returns the connector event log in append order.
func (h *harness) readConnectorEvents(ctx context.Context) ([]connectorEventRecord, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT append_seq, system, delivery_id, event_type, project_ids, correlation,
		        occurred_at, payload, received_at
		   FROM connector_event ORDER BY append_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []connectorEventRecord
	for rows.Next() {
		var (
			record      connectorEventRecord
			correlation []byte
		)
		if err := rows.Scan(&record.AppendSeq, &record.System, &record.DeliveryID,
			&record.EventType, &record.ProjectIDs, &correlation, &record.OccurredAt,
			&record.Payload, &record.ReceivedAt); err != nil {
			return nil, err
		}
		if record.Correlation, err = decodeJSONMap(correlation); err != nil {
			return nil, fmt.Errorf("connector event %d correlation: %w", record.AppendSeq, err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// readCompletionDeliveries returns the completion delivery rows of a run in
// creation order.
func (h *harness) readCompletionDeliveries(ctx context.Context, runID string) ([]completionDeliveryRecord, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT sub_id, run_id, event_id, dedup_key, event, status, attempts,
		        last_error, created_at, delivered_at
		   FROM completion_delivery WHERE run_id = $1 ORDER BY created_at, sub_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []completionDeliveryRecord
	for rows.Next() {
		var record completionDeliveryRecord
		if err := rows.Scan(&record.SubID, &record.RunID, &record.EventID, &record.DedupKey,
			&record.Event, &record.Status, &record.Attempts, &record.LastError,
			&record.CreatedAt, &record.DeliveredAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// extsimDelivery is one callback attempt. StatusCode is 0 when the attempt
// got no HTTP response; Error then carries the connection error.
type extsimDelivery struct {
	DeliveryID string
	Attempt    int
	SentAt     time.Time
	StatusCode int
	Error      string
}

// UnmarshalJSON accepts the field names the control interface may use. The
// v0.2.0 documents fix the meaning of each field but not its JSON spelling,
// so the parser reads the obvious variants and rejects a record with no
// delivery identifier.
func (d *extsimDelivery) UnmarshalJSON(data []byte) error {
	var raw struct {
		DeliveryID string `json:"delivery_id"`
		Delivery   string `json:"delivery"`
		Attempt    *int   `json:"attempt"`
		Seq        *int   `json:"seq"`
		Number     *int   `json:"number"`
		SentAt     string `json:"sent_at"`
		At         string `json:"at"`
		Time       string `json:"time"`
		Timestamp  string `json:"timestamp"`
		StatusCode *int   `json:"status_code"`
		Status     *int   `json:"status"`
		Code       *int   `json:"code"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	switch {
	case raw.DeliveryID != "":
		d.DeliveryID = raw.DeliveryID
	case raw.Delivery != "":
		d.DeliveryID = raw.Delivery
	}
	if d.DeliveryID == "" {
		return fmt.Errorf("delivery record has no delivery_id")
	}

	switch {
	case raw.Attempt != nil:
		d.Attempt = *raw.Attempt
	case raw.Seq != nil:
		d.Attempt = *raw.Seq
	case raw.Number != nil:
		d.Attempt = *raw.Number
	}

	sentAt := raw.SentAt
	if sentAt == "" {
		sentAt = raw.At
	}
	if sentAt == "" {
		sentAt = raw.Time
	}
	if sentAt == "" {
		sentAt = raw.Timestamp
	}
	if sentAt == "" {
		return fmt.Errorf("delivery record %s has no sent_at", d.DeliveryID)
	}
	parsed, err := time.Parse(time.RFC3339Nano, sentAt)
	if err != nil {
		return fmt.Errorf("delivery record %s sent_at %q: %w", d.DeliveryID, sentAt, err)
	}
	d.SentAt = parsed

	switch {
	case raw.StatusCode != nil:
		d.StatusCode = *raw.StatusCode
	case raw.Status != nil:
		d.StatusCode = *raw.Status
	case raw.Code != nil:
		d.StatusCode = *raw.Code
	}
	d.Error = raw.Error
	return nil
}

// parseExtsimDeliveries decodes GET /control/jobs/{id}/deliveries. The
// response may be a bare array or an object with a "deliveries" field.
func parseExtsimDeliveries(data []byte) ([]extsimDelivery, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("the delivery response is empty")
	}

	var records []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &records); err != nil {
			return nil, err
		}
	} else {
		var wrapper struct {
			Deliveries []json.RawMessage `json:"deliveries"`
		}
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return nil, err
		}
		if wrapper.Deliveries == nil {
			return nil, fmt.Errorf("delivery response is neither an array nor an object with a deliveries field")
		}
		records = wrapper.Deliveries
	}

	deliveries := make([]extsimDelivery, 0, len(records))
	for _, record := range records {
		var delivery extsimDelivery
		if err := json.Unmarshal(record, &delivery); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

// ---------------------------------------------------------------------------
// bg_jobs 观察器

// bgJobsChange is one observed state of run.bg_jobs.
type bgJobsChange struct {
	At   time.Time
	Jobs []string
}

// bgJobsObserver polls the background job set and records every change.
type bgJobsObserver struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	changes []bgJobsChange
	err     error
}

// bgJobsReader reads the current set.
type bgJobsReader func(context.Context) ([]string, error)

// watchBGJobs starts observing read every interval. The first read happens
// immediately and is recorded even when the set is empty, so the record has a
// well-defined starting time.
func watchBGJobs(ctx context.Context, interval time.Duration, read bgJobsReader) *bgJobsObserver {
	return watchBGJobsWithClose(ctx, interval, read, nil)
}

func watchBGJobsWithClose(ctx context.Context, interval time.Duration, read bgJobsReader, closeFn func()) *bgJobsObserver {
	if interval <= 0 {
		interval = defaultBGJobsInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	o := &bgJobsObserver{cancel: cancel, done: make(chan struct{})}
	go o.run(ctx, interval, read, closeFn)
	return o
}

func (o *bgJobsObserver) run(ctx context.Context, interval time.Duration, read bgJobsReader, closeFn func()) {
	defer close(o.done)
	if closeFn != nil {
		defer closeFn()
	}

	last, err := read(ctx)
	if err != nil {
		o.setErr(err)
		return
	}
	o.record(last)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, err := read(ctx)
		if err != nil {
			o.setErr(err)
			return
		}
		if slices.Equal(normalizeJobs(last), normalizeJobs(current)) {
			continue
		}
		last = current
		o.record(current)
	}
}

// normalizeJobs sorts a copy so that the observer compares and records sets,
// not arbitrary arrival orders.
func normalizeJobs(jobs []string) []string {
	normalized := slices.Clone(jobs)
	sort.Strings(normalized)
	return normalized
}

func (o *bgJobsObserver) record(jobs []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.changes = append(o.changes, bgJobsChange{At: time.Now().UTC(), Jobs: normalizeJobs(jobs)})
}

func (o *bgJobsObserver) setErr(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.err = err
}

// stop ends the observation and waits for the goroutine to exit.
func (o *bgJobsObserver) stop() {
	o.cancel()
	<-o.done
}

// Changes returns a copy of the recorded states.
func (o *bgJobsObserver) Changes() []bgJobsChange {
	o.mu.Lock()
	defer o.mu.Unlock()
	changes := make([]bgJobsChange, len(o.changes))
	for i, change := range o.changes {
		changes[i] = bgJobsChange{At: change.At, Jobs: slices.Clone(change.Jobs)}
	}
	return changes
}

// Err returns the read error that stopped the observation, if any.
func (o *bgJobsObserver) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// startBGJobsObserver starts observing run.bg_jobs every second, as the
// v0.2.0 criteria require.
func (h *harness) startBGJobsObserver(ctx context.Context, runID string) (*bgJobsObserver, error) {
	pool, err := h.openRunDatabase(ctx)
	if err != nil {
		return nil, err
	}
	read := func(ctx context.Context) ([]string, error) {
		var jobs []string
		err := pool.QueryRow(ctx, `SELECT bg_jobs FROM run WHERE run_id = $1`, runID).Scan(&jobs)
		return jobs, err
	}
	return watchBGJobsWithClose(ctx, defaultBGJobsInterval, read, pool.Close), nil
}

// collectCriteria reads the records every judge takes for a real run. The
// local-mode and Substrate tests use this entry point; unlike
// collectDryRunCriteria it never sets DryRun, so the judges also check the
// suspension records the run produced.
func (h *harness) collectCriteria(t *testing.T, ctx context.Context, runID string, extraJobIDs ...string) criteriaCase {
	t.Helper()

	detail, err := h.getRun(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read run %s: %v", runID, err)
	}
	inbox, err := h.readInbox(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read inbox: %v", err)
	}
	subscriptions, err := h.readSubscriptions(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read subscriptions: %v", err)
	}
	events, err := h.readConnectorEvents(ctx)
	if err != nil {
		t.Fatalf("acceptance: read connector events: %v", err)
	}
	completions, err := h.readCompletionDeliveries(ctx, runID)
	if err != nil {
		t.Fatalf("acceptance: read completion deliveries: %v", err)
	}

	jobIDs := make(map[string]struct{})
	for _, sub := range subscriptions {
		if jobID := criteriaJobID(sub); jobID != "" {
			jobIDs[jobID] = struct{}{}
		}
	}
	for _, jobID := range extraJobIDs {
		jobIDs[jobID] = struct{}{}
	}
	ctl := h.extsimControlClient()
	deliveries := make(map[string][]extsimDelivery, len(jobIDs))
	for jobID := range jobIDs {
		attempts, err := ctl.deliveries(ctx, jobID)
		if err != nil {
			t.Fatalf("acceptance: read extsim deliveries for %s: %v", jobID, err)
		}
		deliveries[jobID] = attempts
	}

	return criteriaCase{
		Turns:                detail.Turns,
		Suspensions:          detail.Suspensions,
		Inbox:                inbox,
		Subscriptions:        subscriptions,
		ConnectorEvents:      events,
		CompletionDeliveries: completions,
		ExtsimDeliveries:     deliveries,
		BGJobsResolution:     defaultBGJobsInterval,
	}
}

// The local-mode and Substrate tests call collectCriteria in a later work
// item. Referencing it here keeps the entry point from being dead code until
// those tests land.
var _ = (*harness).collectCriteria

// ---------------------------------------------------------------------------
// 不依赖真实二进制的框架测试

// TestJudgeFrameworkSigningKey covers the per-run key generation and the
// newline rule connectorhub applies when it reads the same file.
func TestJudgeFrameworkSigningKey(t *testing.T) {
	first, err := newSigningKeyBytes()
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	second, err := newSigningKeyBytes()
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	if len(first) != signingKeySize {
		t.Fatalf("signing key is %d bytes, want %d", len(first), signingKeySize)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two generated signing keys are identical")
	}

	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "plain", content: "abc", want: "abc"},
		{name: "trailing newline", content: "abc\n", want: "abc"},
		{name: "trailing CRLF", content: "abc\r\n", want: "abc"},
		{name: "trailing newline then CRLF", content: "abc\n\r\n", want: "abc\n"},
		{name: "trailing carriage return", content: "abc\r", want: "abc\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write key: %v", err)
			}
			got, err := readSigningKey(path)
			if err != nil {
				t.Fatalf("read signing key: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("signing key = %q, want %q", got, tc.want)
			}
		})
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty key: %v", err)
	}
	if _, err := readSigningKey(empty); err == nil {
		t.Fatal("an empty signing key was accepted")
	}
}

// TestJudgeFrameworkSignature checks signExtsimBody against the HMAC-SHA256
// test vector of RFC 4231, case 1, so the signature is the one the connector
// computes with the standard algorithm.
func TestJudgeFrameworkSignature(t *testing.T) {
	key := bytes.Repeat([]byte{0x0b}, 20)
	const want = "sha256=b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"

	if got := signExtsimBody(key, []byte("Hi There")); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if got := signExtsimBody(key, []byte("Hi There!")); got == want {
		t.Fatal("a changed body kept the signature")
	}
	if got := signExtsimBody([]byte("another-key"), []byte("Hi There")); got == want {
		t.Fatal("a different key kept the signature")
	}
}

// TestJudgeFrameworkConnectorhubConfig checks the generated YAML: the
// definition is the repository's absolute path, the signing key and API base
// URL point at this run, and the binding is extsim: [demo].
func TestJudgeFrameworkConnectorhubConfig(t *testing.T) {
	dir := t.TempDir()
	definition := filepath.Join(dir, "extsim.yaml")
	keyFile := filepath.Join(dir, "extsim-signing-key")
	apiBaseURL := "http://127.0.0.1:9100"

	content := connectorhubConfigYAML(definition, keyFile, apiBaseURL)
	for _, want := range []string{
		"project: default\n",
		"  - definition: " + strconv.Quote(definition) + "\n",
		"    signing_key_file: " + strconv.Quote(keyFile) + "\n",
		"    api_base_url: " + strconv.Quote(apiBaseURL) + "\n",
		"bindings:\n  extsim: [demo]\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("generated configuration does not contain %q:\n%s", want, content)
		}
	}

	wantDefinition := filepath.Join(repoRoot(t), "deploy", "dev", "connectorhub", "extsim.yaml")
	if got := defaultExtsimDefinition(repoRoot(t)); got != wantDefinition {
		t.Fatalf("default definition = %q, want %q", got, wantDefinition)
	}
	if !filepath.IsAbs(defaultExtsimDefinition(repoRoot(t))) {
		t.Fatal("the definition path is not absolute")
	}
}

// TestJudgeFrameworkDeliveryParsing covers the delivery records extsim
// returns: a bare array, an object with a deliveries field, and the field
// name variants the framework accepts.
func TestJudgeFrameworkDeliveryParsing(t *testing.T) {
	sentAt := "2026-11-20T10:00:00.123Z"
	parsedTime, err := time.Parse(time.RFC3339Nano, sentAt)
	if err != nil {
		t.Fatalf("parse fixture time: %v", err)
	}

	t.Run("array", func(t *testing.T) {
		data := []byte(`[{"delivery_id":"d-1","attempt":1,"sent_at":"` + sentAt + `","status_code":200}]`)
		deliveries, err := parseExtsimDeliveries(data)
		if err != nil {
			t.Fatalf("parse deliveries: %v", err)
		}
		if len(deliveries) != 1 {
			t.Fatalf("got %d deliveries, want 1", len(deliveries))
		}
		got := deliveries[0]
		if got.DeliveryID != "d-1" || got.Attempt != 1 || got.StatusCode != 200 || !got.SentAt.Equal(parsedTime) {
			t.Fatalf("delivery = %+v", got)
		}
	})

	t.Run("object with aliases", func(t *testing.T) {
		data := []byte(`{"deliveries":[{"delivery":"d-2","seq":2,"at":"` + sentAt + `","status":503,"error":"unavailable"}]}`)
		deliveries, err := parseExtsimDeliveries(data)
		if err != nil {
			t.Fatalf("parse deliveries: %v", err)
		}
		if len(deliveries) != 1 {
			t.Fatalf("got %d deliveries, want 1", len(deliveries))
		}
		got := deliveries[0]
		if got.DeliveryID != "d-2" || got.Attempt != 2 || got.StatusCode != 503 || got.Error != "unavailable" {
			t.Fatalf("delivery = %+v", got)
		}
	})

	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "empty body", data: ""},
		{name: "invalid JSON", data: `{`},
		{name: "object without deliveries", data: `{"other":[]}`},
		{name: "missing delivery id", data: `[{"sent_at":"` + sentAt + `"}]`},
		{name: "missing sent at", data: `[{"delivery_id":"d-3"}]`},
		{name: "bad sent at", data: `[{"delivery_id":"d-4","sent_at":"yesterday"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseExtsimDeliveries([]byte(tc.data)); err == nil {
				t.Fatal("the malformed delivery response was accepted")
			}
		})
	}
}

// TestJudgeFrameworkBGJobsObserver checks that the observer reads every
// second-scale interval, records the starting state, and timestamps each
// change, comparing the set rather than the order.
func TestJudgeFrameworkBGJobsObserver(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	read := func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch {
		case calls <= 2:
			return []string{"job-b", "job-a"}, nil
		case calls <= 5:
			// Same set, different order: not a change.
			return []string{"job-a", "job-b"}, nil
		case calls <= 8:
			return []string{"job-c", "job-a", "job-b"}, nil
		default:
			return nil, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := watchBGJobs(ctx, 5*time.Millisecond, read)
	defer o.stop()

	deadline := time.Now().Add(10 * time.Second)
	for len(o.Changes()) < 3 {
		if err := o.Err(); err != nil {
			t.Fatalf("observer stopped: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer recorded %d changes, want 3: %+v", len(o.Changes()), o.Changes())
		}
		time.Sleep(time.Millisecond)
	}

	changes := o.Changes()
	want := [][]string{
		{"job-a", "job-b"},
		{"job-a", "job-b", "job-c"},
	}
	// The observer records the initial read and one record per change. The
	// third record is the empty set, which normalizeJobs turns into an empty
	// non-nil slice.
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3: %+v", len(changes), changes)
	}
	if !slices.Equal(changes[0].Jobs, want[0]) {
		t.Fatalf("initial set = %v, want %v", changes[0].Jobs, want[0])
	}
	if !slices.Equal(changes[1].Jobs, want[1]) {
		t.Fatalf("second set = %v, want %v", changes[1].Jobs, want[1])
	}
	if len(changes[2].Jobs) != 0 {
		t.Fatalf("third set = %v, want empty", changes[2].Jobs)
	}
	for i := 1; i < len(changes); i++ {
		if !changes[i].At.After(changes[i-1].At) {
			t.Fatalf("change %d at %s is not after change %d at %s", i, changes[i].At, i-1, changes[i-1].At)
		}
	}
}
