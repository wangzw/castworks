package connectorhub

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Definition 是 kind: ExternalSystem 的外部系统定义，字段与设计文档
// “外部系统定义”一节的闭集一致。v0.2.0 只使用其中一部分字段，其余字段
// 照常解析以接受完整定义，但不参与本模块的行为。
type Definition struct {
	Kind           string          `yaml:"kind"`
	Name           string          `yaml:"name"`
	Auth           *Auth           `yaml:"auth"`
	Access         *Access         `yaml:"access"`
	Events         Events          `yaml:"events"`
	WaitSignatures []WaitSignature `yaml:"wait_signatures"`
}

// Auth 是外部系统的认证声明。v0.2.0 不使用它，可以省略。
type Auth struct {
	Methods      []string       `yaml:"methods"`
	SecretSchema map[string]any `yaml:"secret_schema"`
	TokenMinting *TokenMinting  `yaml:"token_minting"`
}

// TokenMinting 是短期凭据的兑换规则。
type TokenMinting struct {
	TTL    string   `yaml:"ttl"`
	Scopes []string `yaml:"scopes"`
}

// Access 是外部系统的网络通道声明。v0.2.0 的出口策略由 sandboxprovider
// 生成，本模块解析这一段但不使用。
type Access struct {
	Mode           string       `yaml:"mode"`
	Upstreams      []string     `yaml:"upstreams"`
	HostModes      []HostMode   `yaml:"host_modes"`
	PinnedHosts    []string     `yaml:"pinned_hosts"`
	PlaintextHosts []string     `yaml:"plaintext_hosts"`
	Inject         []InjectRule `yaml:"inject"`
	Git            *GitRule     `yaml:"git"`
}

// HostMode 按主机覆盖默认的网络通道。
type HostMode struct {
	Host string `yaml:"host"`
	Mode string `yaml:"mode"`
}

// InjectRule 描述一条请求头占位符替换规则。
type InjectRule struct {
	Host        string   `yaml:"host"`
	Header      string   `yaml:"header"`
	Placeholder string   `yaml:"placeholder"`
	AuthMethod  string   `yaml:"auth_method"`
	Scopes      []string `yaml:"scopes"`
}

// GitRule 是 git 通道的规则。
type GitRule struct {
	Mode string `yaml:"mode"`
}

// Events 是外部系统的事件声明。
type Events struct {
	Sources                  []EventSource         `yaml:"sources"`
	CorrelationKeys          []string              `yaml:"correlation_keys"`
	IdentityKeys             []string              `yaml:"identity_keys"`
	ProjectBinding           string                `yaml:"project_binding"`
	CorrelationNormalization map[string]string     `yaml:"correlation_normalization"`
	DedupKey                 []string              `yaml:"dedup_key"`
	Ordering                 Ordering              `yaml:"ordering"`
	Completions              []CompletionCondition `yaml:"completions"`
}

// EventSource 是一个事件源。v0.2.0 只接受 webhook 类型。
type EventSource struct {
	Type       string   `yaml:"type"`
	Path       string   `yaml:"path"`
	Signature  string   `yaml:"signature"`
	DeliveryID string   `yaml:"delivery_id"`
	EventTypes []string `yaml:"event_types"`
	Interval   string   `yaml:"interval"`
	Kind       string   `yaml:"kind"`
}

// Ordering 是事件发生时间等排序依据的字段路径。
type Ordering struct {
	OccurredAt string `yaml:"occurred_at"`
}

// CompletionCondition 是一条完成条件：事件类型与终态核对方式。
type CompletionCondition struct {
	Event  string  `yaml:"event"`
	Verify *Verify `yaml:"verify"`
}

// Verify 是只读状态查询与终态取值闭集。
type Verify struct {
	API      string   `yaml:"api"`
	Terminal []string `yaml:"terminal"`
}

// WaitSignature 是一条等待签名。v0.2.0 不使用它，只照常解析。
type WaitSignature struct {
	ID          string            `yaml:"id"`
	Match       WaitMatch         `yaml:"match"`
	EventType   string            `yaml:"event_type"`
	Correlation map[string]string `yaml:"correlation"`
	Prior       *Prior            `yaml:"prior"`
}

// WaitMatch 是一条等待签名的匹配规则，command 与 request 二选一。
type WaitMatch struct {
	Command string        `yaml:"command"`
	Request *RequestMatch `yaml:"request"`
}

// RequestMatch 是按出口请求匹配的规则。
type RequestMatch struct {
	Host      string `yaml:"host"`
	Method    string `yaml:"method"`
	PathRegex string `yaml:"path_regex"`
}

// Prior 是等待时长的先验。
type Prior struct {
	P25 string `yaml:"p25"`
	P50 string `yaml:"p50"`
	P90 string `yaml:"p90"`
}

const (
	definitionKind = "ExternalSystem"

	// signatureHMACSHA256 是 v0.2.0 唯一支持的 webhook 签名方式。
	signatureHMACSHA256 = "hmac-sha256"
	// normalizationLowercase 是 v0.2.0 唯一支持的关联键规范化函数。
	normalizationLowercase = "lowercase"
)

// validate 校验定义中 v0.2.0 使用的字段。字段闭集由严格解码保证，这里只
// 检查取值与必填关系。
func (d *Definition) validate() error {
	if d.Kind != definitionKind {
		return fmt.Errorf("kind must be %q", definitionKind)
	}
	if d.Name == "" {
		return errors.New("name is required")
	}
	return d.Events.validate()
}

func (e *Events) validate() error {
	if len(e.Sources) == 0 {
		return errors.New("events.sources is required")
	}

	produced := make(map[string]bool)
	webhook := false
	for i := range e.Sources {
		source := &e.Sources[i]
		switch source.Type {
		case "webhook":
			webhook = true
			if err := source.validate(); err != nil {
				return fmt.Errorf("events.sources[%d]: %w", i, err)
			}
			for _, eventType := range source.EventTypes {
				produced[eventType] = true
			}
		case "poll":
			return fmt.Errorf("events.sources[%d]: poll event sources are not supported", i)
		default:
			return fmt.Errorf("events.sources[%d]: unknown type %q", i, source.Type)
		}
	}
	if !webhook {
		return errors.New("events.sources must contain a webhook event source")
	}

	keys := make(map[string]bool, len(e.CorrelationKeys))
	if len(e.CorrelationKeys) == 0 {
		return errors.New("events.correlation_keys is required")
	}
	for _, key := range e.CorrelationKeys {
		if key == "" {
			return errors.New("events.correlation_keys contains an empty key")
		}
		keys[key] = true
	}

	if len(e.IdentityKeys) == 0 {
		return errors.New("events.identity_keys is required")
	}
	for _, key := range e.IdentityKeys {
		if !keys[key] {
			return fmt.Errorf("events.identity_keys %q is not in correlation_keys", key)
		}
	}

	if e.ProjectBinding == "" {
		return errors.New("events.project_binding is required with a webhook event source")
	}
	if !keys[e.ProjectBinding] {
		return fmt.Errorf("events.project_binding %q is not in correlation_keys", e.ProjectBinding)
	}

	if len(e.DedupKey) == 0 {
		return errors.New("events.dedup_key is required")
	}

	if e.Ordering.OccurredAt == "" {
		return errors.New("events.ordering.occurred_at is required")
	}

	for i := range e.Completions {
		if err := e.Completions[i].validate(produced); err != nil {
			return fmt.Errorf("events.completions[%d]: %w", i, err)
		}
	}

	for key, function := range e.CorrelationNormalization {
		if !keys[key] {
			return fmt.Errorf("events.correlation_normalization key %q is not in correlation_keys", key)
		}
		if function != normalizationLowercase {
			return fmt.Errorf("events.correlation_normalization[%q]: unsupported function %q", key, function)
		}
	}

	return nil
}

func (s *EventSource) validate() error {
	if s.Path == "" {
		return errors.New("path is required")
	}
	if s.Signature == "" {
		return errors.New("signature is required")
	}
	if s.Signature != signatureHMACSHA256 {
		return fmt.Errorf("unsupported signature %q", s.Signature)
	}
	if s.DeliveryID == "" {
		return errors.New("delivery_id is required")
	}
	if len(s.EventTypes) == 0 {
		return errors.New("event_types is required")
	}
	for _, eventType := range s.EventTypes {
		if eventType == "" {
			return errors.New("event_types contains an empty value")
		}
	}
	return nil
}

func (c *CompletionCondition) validate(produced map[string]bool) error {
	if c.Event == "" {
		return errors.New("event is required")
	}
	if !produced[c.Event] {
		return fmt.Errorf("event %q is not produced by events.sources", c.Event)
	}
	if c.Verify == nil {
		return errors.New("verify is required")
	}
	if c.Verify.API == "" {
		return errors.New("verify.api is required")
	}
	if len(c.Verify.Terminal) == 0 {
		return errors.New("verify.terminal is required")
	}
	for _, terminal := range c.Verify.Terminal {
		if terminal == "" {
			return errors.New("verify.terminal contains an empty value")
		}
	}
	return nil
}

// loadDefinition 读取、严格解码并校验一个外部系统定义文件。
func loadDefinition(path string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("read definition %s: %w", path, err)
	}
	var definition Definition
	if err := decodeStrictYAML(data, &definition); err != nil {
		return Definition{}, fmt.Errorf("parse definition %s: %w", path, err)
	}
	if err := definition.validate(); err != nil {
		return Definition{}, fmt.Errorf("definition %s: %w", path, err)
	}
	return definition, nil
}

// Normalize 按定义对某个关联键的取值做规范化。没有为该键声明规范化函数
// 时原样返回。
func (d Definition) Normalize(key, value string) string {
	if d.Events.CorrelationNormalization[key] == normalizationLowercase {
		return strings.ToLower(value)
	}
	return value
}

// NormalizeCorrelation 返回按定义规范化后的关联键副本，不修改入参。
func (d Definition) NormalizeCorrelation(correlation map[string]string) map[string]string {
	normalized := make(map[string]string, len(correlation))
	for key, value := range correlation {
		normalized[key] = d.Normalize(key, value)
	}
	return normalized
}

// CompletionFor 返回某个事件类型的完成条件。
func (d Definition) CompletionFor(event string) (*CompletionCondition, bool) {
	for i := range d.Events.Completions {
		if d.Events.Completions[i].Event == event {
			return &d.Events.Completions[i], true
		}
	}
	return nil, false
}

// EventTypes 返回事件目录投影的事件类型：webhook 事件源产生的事件类型与
// 完成条件的并集，按字典序排列。
func (d Definition) EventTypes() []string {
	seen := make(map[string]bool)
	for _, source := range d.Events.Sources {
		for _, eventType := range source.EventTypes {
			seen[eventType] = true
		}
	}
	for _, completion := range d.Events.Completions {
		seen[completion.Event] = true
	}
	eventTypes := make([]string, 0, len(seen))
	for eventType := range seen {
		eventTypes = append(eventTypes, eventType)
	}
	sort.Strings(eventTypes)
	return eventTypes
}
