// Package extsim 实现 extsim 模拟外部系统的连接器。
//
// 连接器负责设计中“由连接器规范化”的部分：校验 webhook 签名、读取事件
// 类型、从回调负载取出结论字段，以及按订阅关联键查询作业状态。它只依赖
// 标准库，不引用 pkg/connectorhub；后者按外部系统定义名 extsim 构造。
package extsim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// HeaderSignature 是 webhook 签名头的名字。
	HeaderSignature = "X-Extsim-Signature-256"
	// HeaderEvent 是 webhook 事件类型头的名字。
	HeaderEvent = "X-Extsim-Event"

	// EventJobCompleted 是 extsim 产生的事件类型。
	EventJobCompleted = "job.completed"
	// APIJob 是 extsim 状态查询在定义中的 api 名称。
	APIJob = "extsim-job"

	signaturePrefix = "sha256="

	defaultStatusTimeout   = 5 * time.Second
	maxStatusResponseBytes = 1 << 20
)

// Connector 是 extsim 的连接器实现。
type Connector struct {
	apiBaseURL    string
	client        *http.Client
	statusTimeout time.Duration
}

// New 构造 extsim 连接器。apiBaseURL 是状态查询的基地址；client 为空时
// 使用不带全局状态的默认客户端，状态查询的 5 s 超时由 CheckStatus 自己
// 施加的 context 保证。
func New(apiBaseURL string, client *http.Client) *Connector {
	if client == nil {
		client = &http.Client{}
	}
	return &Connector{
		apiBaseURL:    strings.TrimRight(apiBaseURL, "/"),
		client:        client,
		statusTimeout: defaultStatusTimeout,
	}
}

// VerifySignature 校验请求头 X-Extsim-Signature-256。取值是 sha256= 加请求
// 体 HMAC-SHA256 的十六进制值，比较以常数时间进行。缺头、前缀不符、摘要
// 长度或内容不符时返回 false。
func (*Connector) VerifySignature(h http.Header, body, key []byte) bool {
	value := h.Get(HeaderSignature)
	if value == "" {
		return false
	}
	encoded, ok := strings.CutPrefix(value, signaturePrefix)
	if !ok {
		return false
	}
	got, err := hex.DecodeString(encoded)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// EventType 读取请求头 X-Extsim-Event。extsim 只产生 job.completed，其他
// 取值按未知事件类型拒绝。
func (*Connector) EventType(h http.Header) (string, error) {
	event := strings.TrimSpace(h.Get(HeaderEvent))
	if event == "" {
		return "", fmt.Errorf("missing %s header", HeaderEvent)
	}
	if event != EventJobCompleted {
		return "", fmt.Errorf("unsupported event type %q", event)
	}
	return event, nil
}

// Terminal 从 job.completed 的回调负载取 job.conclusion。结论缺失时返回
// 错误，调用方据此拒绝这次事件。
func (*Connector) Terminal(body []byte) (string, error) {
	var payload struct {
		Job struct {
			Conclusion string `json:"conclusion"`
		} `json:"job"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse webhook payload: %w", err)
	}
	if payload.Job.Conclusion == "" {
		return "", errors.New("webhook payload has no job.conclusion")
	}
	return payload.Job.Conclusion, nil
}

// CheckStatus 按关联键中的 job.id 查询 GET {api_base_url}/v1/jobs/{id}，
// 自带 5 s 超时。作业 status 为 completed 时 reached 为真，terminal 取
// conclusion，body 为响应体原文；其他 status 表示未到终态，reached 为假
// 且不返回错误。404、超时、连接失败与其他非 200 响应都返回非空错误，表示
// 不可达。
func (c *Connector) CheckStatus(ctx context.Context, api string, correlation map[string]string) (terminal string, reached bool, body []byte, err error) {
	if api != APIJob {
		return "", false, nil, fmt.Errorf("unsupported api %q", api)
	}
	id := correlation["job.id"]
	if id == "" {
		return "", false, nil, errors.New("correlation has no job.id")
	}

	ctx, cancel := context.WithTimeout(ctx, c.statusTimeout)
	defer cancel()

	endpoint := c.apiBaseURL + "/v1/jobs/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, nil, fmt.Errorf("build job status request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", false, nil, fmt.Errorf("query job status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxStatusResponseBytes+1))
	if err != nil {
		return "", false, nil, fmt.Errorf("read job status response: %w", err)
	}
	if len(body) > maxStatusResponseBytes {
		return "", false, nil, fmt.Errorf("job status response exceeds %d bytes", maxStatusResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, nil, fmt.Errorf("query job status: unexpected status %s", resp.Status)
	}

	var job struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(body, &job); err != nil {
		return "", false, body, fmt.Errorf("parse job status response: %w", err)
	}
	if job.Status != "completed" {
		return "", false, body, nil
	}
	if job.Conclusion == "" {
		return "", false, body, errors.New("job status response has no conclusion")
	}
	return job.Conclusion, true, body, nil
}
