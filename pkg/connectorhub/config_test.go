package connectorhub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testdataDefinition 读取照抄 extsim.md 的外部系统定义。
func testdataDefinition(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "extsim.yaml"))
	if err != nil {
		t.Fatalf("read testdata definition: %v", err)
	}
	return string(data)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLoadConfigTestdataDefinition 确认 testdata 下照抄 extsim.md 的定义能够
// 装载，且 v0.2.0 使用的字段解析正确。
func TestLoadConfigTestdataDefinition(t *testing.T) {
	definition, err := loadDefinition(filepath.Join("testdata", "extsim.yaml"))
	if err != nil {
		t.Fatalf("loadDefinition: %v", err)
	}

	if definition.Kind != "ExternalSystem" {
		t.Errorf("Kind = %q, want ExternalSystem", definition.Kind)
	}
	if definition.Name != "extsim" {
		t.Errorf("Name = %q, want extsim", definition.Name)
	}
	if len(definition.Events.Sources) != 1 {
		t.Fatalf("Sources = %d, want 1", len(definition.Events.Sources))
	}
	source := definition.Events.Sources[0]
	if source.Type != "webhook" || source.Path != "/hooks/extsim" {
		t.Errorf("source = %+v, want webhook /hooks/extsim", source)
	}
	if source.Signature != "hmac-sha256" || source.DeliveryID != "X-Extsim-Delivery" {
		t.Errorf("source signature/delivery = %q/%q", source.Signature, source.DeliveryID)
	}
	if !slices.Equal(definition.Events.CorrelationKeys, []string{"job.project", "job.id"}) {
		t.Errorf("CorrelationKeys = %v", definition.Events.CorrelationKeys)
	}
	if !slices.Equal(definition.Events.IdentityKeys, []string{"job.id"}) {
		t.Errorf("IdentityKeys = %v", definition.Events.IdentityKeys)
	}
	if definition.Events.ProjectBinding != "job.project" {
		t.Errorf("ProjectBinding = %q", definition.Events.ProjectBinding)
	}
	if definition.Events.Ordering.OccurredAt != "job.updated_at" {
		t.Errorf("OccurredAt = %q", definition.Events.Ordering.OccurredAt)
	}
	if !slices.Equal(definition.EventTypes(), []string{"job.completed"}) {
		t.Errorf("EventTypes = %v", definition.EventTypes())
	}

	completion, ok := definition.CompletionFor("job.completed")
	if !ok {
		t.Fatal("CompletionFor(job.completed) not found")
	}
	if completion.Verify == nil || completion.Verify.API != "extsim-job" {
		t.Errorf("Verify = %+v", completion.Verify)
	}
	if !slices.Equal(completion.Verify.Terminal, []string{"succeeded", "failed"}) {
		t.Errorf("Terminal = %v", completion.Verify.Terminal)
	}
	if _, ok := definition.CompletionFor("job.failed"); ok {
		t.Error("CompletionFor(job.failed) unexpectedly found")
	}

	// access 解析后不使用，但字段仍然可读。
	if definition.Access == nil || definition.Access.Mode != "passthrough" {
		t.Errorf("Access = %+v", definition.Access)
	}
	if definition.Auth != nil {
		t.Errorf("Auth = %+v, want nil", definition.Auth)
	}
}

// TestLoadConfigParsesUnusedSections 确认 auth 与 wait_signatures 可以省略或
// 出现，出现时照常解析但不影响装载，correlation_normalization 的 lowercase
// 生效。
func TestLoadConfigParsesUnusedSections(t *testing.T) {
	base := testdataDefinition(t)
	definitionYAML := base +
		`auth:
  methods: [token]
  secret_schema: {token: {type: string}}
  token_minting: {ttl: 1h, scopes: [read]}
wait_signatures:
  - id: sim-wait
    match: {command: '\bjob\s+watch\s+(?P<id>\S+)'}
    event_type: job.completed
    correlation: {job.id: '${id}'}
    prior: {p25: 2m, p50: 6m, p90: 20m}
`
	definitionYAML = strings.Replace(definitionYAML,
		"  project_binding: job.project\n",
		"  project_binding: job.project\n  correlation_normalization: {job.id: lowercase}\n", 1)

	dir := t.TempDir()
	path := filepath.Join(dir, "extsim.yaml")
	writeTestFile(t, path, definitionYAML)

	definition, err := loadDefinition(path)
	if err != nil {
		t.Fatalf("loadDefinition: %v", err)
	}
	if definition.Auth == nil || !slices.Equal(definition.Auth.Methods, []string{"token"}) {
		t.Errorf("Auth = %+v", definition.Auth)
	}
	if len(definition.WaitSignatures) != 1 || definition.WaitSignatures[0].ID != "sim-wait" {
		t.Errorf("WaitSignatures = %+v", definition.WaitSignatures)
	}
	if got := definition.Normalize("job.id", "JOB-ABC"); got != "job-abc" {
		t.Errorf("Normalize(job.id) = %q, want job-abc", got)
	}
	if got := definition.Normalize("job.project", "DEMO"); got != "DEMO" {
		t.Errorf("Normalize(job.project) = %q, want DEMO", got)
	}
	normalized := definition.NormalizeCorrelation(map[string]string{"job.id": "JOB-1", "job.project": "Demo"})
	if normalized["job.id"] != "job-1" || normalized["job.project"] != "Demo" {
		t.Errorf("NormalizeCorrelation = %v", normalized)
	}
}

func TestLoadConfigFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "extsim.yaml"), testdataDefinition(t))
	writeTestFile(t, filepath.Join(dir, "signing.key"), "signing-key\n")
	configPath := filepath.Join(dir, "connectorhub.yaml")
	writeTestFile(t, configPath, `project: default
systems:
  - definition: extsim.yaml
    signing_key_file: signing.key
    api_base_url: http://127.0.0.1:9100
bindings:
  extsim: [demo]
`)

	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.Project != "default" {
		t.Errorf("Project = %q", config.Project)
	}
	if len(config.Systems) != 1 {
		t.Fatalf("Systems = %d, want 1", len(config.Systems))
	}

	system := config.Systems[0]
	if system.Name != "extsim" {
		t.Errorf("Name = %q", system.Name)
	}
	if system.DefinitionPath != filepath.Join(dir, "extsim.yaml") {
		t.Errorf("DefinitionPath = %q", system.DefinitionPath)
	}
	if system.SigningKeyFile != filepath.Join(dir, "signing.key") {
		t.Errorf("SigningKeyFile = %q", system.SigningKeyFile)
	}
	if system.APIBaseURL != "http://127.0.0.1:9100" {
		t.Errorf("APIBaseURL = %q", system.APIBaseURL)
	}
	if system.Connector == nil {
		t.Error("Connector is nil")
	}
	if !slices.Equal(system.BindingValues, []string{"demo"}) {
		t.Errorf("BindingValues = %v", system.BindingValues)
	}
	if !slices.Equal(config.Bindings["extsim"], []string{"demo"}) {
		t.Errorf("Bindings[extsim] = %v", config.Bindings["extsim"])
	}

	byName, ok := config.System("extsim")
	if !ok || byName != &config.Systems[0] {
		t.Errorf("System(extsim) = %v, %v", byName, ok)
	}
	if _, ok := config.System("missing"); ok {
		t.Error("System(missing) unexpectedly found")
	}

	byPath, ok := config.SystemByWebhookPath("/hooks/extsim")
	if !ok || byPath != &config.Systems[0] {
		t.Errorf("SystemByWebhookPath(/hooks/extsim) = %v, %v", byPath, ok)
	}
	if _, ok := config.SystemByWebhookPath("/hooks/other"); ok {
		t.Error("SystemByWebhookPath(/hooks/other) unexpectedly found")
	}

	if got := config.ProjectsForBinding("extsim", "demo"); !slices.Equal(got, []string{"default"}) {
		t.Errorf("ProjectsForBinding(demo) = %v", got)
	}
	if got := config.ProjectsForBinding("extsim", "other"); got != nil {
		t.Errorf("ProjectsForBinding(other) = %v, want nil", got)
	}
}

func TestLoadConfigResolvesPaths(t *testing.T) {
	t.Run("relative to config directory", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "definitions", "extsim.yaml"), testdataDefinition(t))
		writeTestFile(t, filepath.Join(dir, "keys", "extsim.key"), "key\n")
		configPath := filepath.Join(dir, "connectorhub.yaml")
		writeTestFile(t, configPath, `project: default
systems:
  - definition: definitions/extsim.yaml
    signing_key_file: keys/extsim.key
`)

		config, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got, want := config.Systems[0].DefinitionPath, filepath.Join(dir, "definitions", "extsim.yaml"); got != want {
			t.Errorf("DefinitionPath = %q, want %q", got, want)
		}
		if got, want := config.Systems[0].SigningKeyFile, filepath.Join(dir, "keys", "extsim.key"); got != want {
			t.Errorf("SigningKeyFile = %q, want %q", got, want)
		}
	})

	t.Run("home directory", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeTestFile(t, filepath.Join(home, ".config", "castworks", "extsim.yaml"), testdataDefinition(t))
		writeTestFile(t, filepath.Join(home, ".config", "castworks", "extsim-signing-key"), "key\n")

		dir := t.TempDir()
		configPath := filepath.Join(dir, "connectorhub.yaml")
		writeTestFile(t, configPath, `project: default
systems:
  - definition: ~/.config/castworks/extsim.yaml
    signing_key_file: ~/.config/castworks/extsim-signing-key
`)

		config, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		wantDefinition := filepath.Join(home, ".config", "castworks", "extsim.yaml")
		if got := config.Systems[0].DefinitionPath; got != wantDefinition {
			t.Errorf("DefinitionPath = %q, want %q", got, wantDefinition)
		}
		wantKey := filepath.Join(home, ".config", "castworks", "extsim-signing-key")
		if got := config.Systems[0].SigningKeyFile; got != wantKey {
			t.Errorf("SigningKeyFile = %q, want %q", got, wantKey)
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		definitionDir := t.TempDir()
		definitionPath := filepath.Join(definitionDir, "extsim.yaml")
		writeTestFile(t, definitionPath, testdataDefinition(t))

		dir := t.TempDir()
		configPath := filepath.Join(dir, "connectorhub.yaml")
		writeTestFile(t, configPath, "project: default\nsystems:\n  - definition: "+definitionPath+"\n")

		config, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if got := config.Systems[0].DefinitionPath; got != definitionPath {
			t.Errorf("DefinitionPath = %q, want %q", got, definitionPath)
		}
	})
}

// TestLoadConfigRejectsInvalidDefinition 对每种拒绝装载的定义各测一条。
func TestLoadConfigRejectsInvalidDefinition(t *testing.T) {
	base := testdataDefinition(t)

	const webhookSource = `  sources:
    - type: webhook
      path: /hooks/extsim
      signature: hmac-sha256
      delivery_id: X-Extsim-Delivery
      event_types: [job.completed]
`

	tests := []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{
			name:    "unknown field",
			old:     "kind: ExternalSystem\n",
			new:     "kind: ExternalSystem\nunknown_field: x\n",
			wantErr: "unknown_field",
		},
		{
			name:    "wrong kind",
			old:     "kind: ExternalSystem",
			new:     "kind: Other",
			wantErr: "kind must be",
		},
		{
			name:    "empty name",
			old:     "name: extsim",
			new:     `name: ""`,
			wantErr: "name is required",
		},
		{
			name:    "no event sources",
			old:     webhookSource,
			new:     "  sources: []\n",
			wantErr: "events.sources is required",
		},
		{
			name: "poll event source",
			old:  webhookSource,
			new: `  sources:
    - type: poll
      interval: 60s
      kind: state_reconcile
`,
			wantErr: "poll event sources are not supported",
		},
		{
			name:    "unknown event source type",
			old:     "type: webhook",
			new:     "type: carriermail",
			wantErr: "unknown type",
		},
		{
			name:    "missing webhook path",
			old:     "      path: /hooks/extsim\n",
			new:     "",
			wantErr: "path is required",
		},
		{
			name:    "unsupported signature",
			old:     "signature: hmac-sha256",
			new:     "signature: hmac-sha1",
			wantErr: "unsupported signature",
		},
		{
			name:    "missing delivery id",
			old:     "      delivery_id: X-Extsim-Delivery\n",
			new:     "",
			wantErr: "delivery_id is required",
		},
		{
			name:    "empty event types",
			old:     "event_types: [job.completed]",
			new:     "event_types: []",
			wantErr: "event_types is required",
		},
		{
			name:    "empty correlation keys",
			old:     "correlation_keys: [job.project, job.id]",
			new:     "correlation_keys: []",
			wantErr: "events.correlation_keys is required",
		},
		{
			name:    "empty identity keys",
			old:     "identity_keys: [job.id]",
			new:     "identity_keys: []",
			wantErr: "events.identity_keys is required",
		},
		{
			name:    "identity key not in correlation keys",
			old:     "identity_keys: [job.id]",
			new:     "identity_keys: [job.owner]",
			wantErr: "is not in correlation_keys",
		},
		{
			name:    "missing project binding",
			old:     "  project_binding: job.project\n",
			new:     "",
			wantErr: "events.project_binding is required",
		},
		{
			name:    "project binding not in correlation keys",
			old:     "project_binding: job.project",
			new:     "project_binding: job.owner",
			wantErr: "is not in correlation_keys",
		},
		{
			name:    "empty dedup key",
			old:     "dedup_key: [event_type, job.id, job.conclusion]",
			new:     "dedup_key: []",
			wantErr: "events.dedup_key is required",
		},
		{
			name:    "missing occurred_at",
			old:     "ordering: {occurred_at: job.updated_at}",
			new:     "ordering: {}",
			wantErr: "events.ordering.occurred_at is required",
		},
		{
			name:    "completion without event",
			old:     "    - event: job.completed\n",
			new:     "    - event: \"\"\n",
			wantErr: "event is required",
		},
		{
			name:    "completion without verify",
			old:     "      verify: {api: extsim-job, terminal: [succeeded, failed]}\n",
			new:     "",
			wantErr: "verify is required",
		},
		{
			name:    "verify without api",
			old:     "verify: {api: extsim-job, terminal: [succeeded, failed]}",
			new:     "verify: {terminal: [succeeded, failed]}",
			wantErr: "verify.api is required",
		},
		{
			name:    "verify with empty terminal",
			old:     "verify: {api: extsim-job, terminal: [succeeded, failed]}",
			new:     "verify: {api: extsim-job, terminal: []}",
			wantErr: "verify.terminal is required",
		},
		{
			name:    "completion event not produced by sources",
			old:     "    - event: job.completed\n",
			new:     "    - event: job.failed\n",
			wantErr: "is not produced by events.sources",
		},
		{
			name:    "unsupported normalization function",
			old:     "  project_binding: job.project\n",
			new:     "  project_binding: job.project\n  correlation_normalization: {job.id: uppercase}\n",
			wantErr: "unsupported function",
		},
		{
			name:    "normalization key not in correlation keys",
			old:     "  project_binding: job.project\n",
			new:     "  project_binding: job.project\n  correlation_normalization: {job.owner: lowercase}\n",
			wantErr: "is not in correlation_keys",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := strings.Replace(base, tt.old, tt.new, 1)
			if mutated == base {
				t.Fatalf("mutation %q did not change the definition", tt.old)
			}
			path := filepath.Join(t.TempDir(), "extsim.yaml")
			writeTestFile(t, path, mutated)

			_, err := loadDefinition(path)
			if err == nil {
				t.Fatalf("loadDefinition succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadConfigRejectsInvalidConfig 覆盖配置层面的拒绝情形。
func TestLoadConfigRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) string
		wantErr string
	}{
		{
			name: "missing project",
			setup: func(t *testing.T, dir string) string {
				writeTestFile(t, filepath.Join(dir, "extsim.yaml"), testdataDefinition(t))
				return "systems:\n  - definition: extsim.yaml\n"
			},
			wantErr: "project is required",
		},
		{
			name: "unknown config field",
			setup: func(t *testing.T, dir string) string {
				writeTestFile(t, filepath.Join(dir, "extsim.yaml"), testdataDefinition(t))
				return "project: default\nextra: true\nsystems:\n  - definition: extsim.yaml\n"
			},
			wantErr: "extra",
		},
		{
			name: "missing definition path",
			setup: func(t *testing.T, dir string) string {
				return "project: default\nsystems:\n  - signing_key_file: key\n"
			},
			wantErr: "definition is required",
		},
		{
			name: "missing definition file",
			setup: func(t *testing.T, dir string) string {
				return "project: default\nsystems:\n  - definition: missing.yaml\n"
			},
			wantErr: "read definition",
		},
		{
			name: "no connector for definition name",
			setup: func(t *testing.T, dir string) string {
				definition := strings.Replace(testdataDefinition(t), "name: extsim", "name: other-system", 1)
				writeTestFile(t, filepath.Join(dir, "other.yaml"), definition)
				return "project: default\nsystems:\n  - definition: other.yaml\n"
			},
			wantErr: "no connector for system",
		},
		{
			name: "duplicate system name",
			setup: func(t *testing.T, dir string) string {
				writeTestFile(t, filepath.Join(dir, "extsim.yaml"), testdataDefinition(t))
				return "project: default\nsystems:\n  - definition: extsim.yaml\n  - definition: extsim.yaml\n"
			},
			wantErr: "duplicate system name",
		},
		{
			name: "binding for unknown system",
			setup: func(t *testing.T, dir string) string {
				return "project: default\nsystems: []\nbindings:\n  other: [value]\n"
			},
			wantErr: "bindings reference unknown system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configContent := tt.setup(t, dir)
			configPath := filepath.Join(dir, "connectorhub.yaml")
			writeTestFile(t, configPath, configContent)

			_, err := LoadConfig(configPath)
			if err == nil {
				t.Fatalf("LoadConfig succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("LoadConfig succeeded, want error")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("error = %v, want containing read config", err)
	}
}
