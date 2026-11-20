//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// v0.2.0 验收使用的外部系统是 cmd/extsim。它只以子进程方式使用：测试不在
// 进程内 import 它，也不与它共享内存。本文件按 extsim.md、connectorhub.md
// 与 runcontroller.md 的接口把调用写出来，上游实现合入前只保证编译与不依赖
// 真实二进制的框架测试通过。

const (
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
// 数据库读取

// inboxRecord is one row of the inbox table.
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
