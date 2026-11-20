package connectorhub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config 是控制面参数 --connectorhub-config 指向的配置。外部系统定义在装载
// 时按字段闭集解析并校验，路径按配置文件所在目录解析。
type Config struct {
	// Project 是唯一的项目，所有运行实例都属于它。
	Project string
	// Systems 是装载成功的外部系统。
	Systems []System
	// Bindings 是外部系统名到项目归属键取值的映射。取值已按定义规范化。
	Bindings map[string][]string

	byName map[string]int
	byPath map[string]int
}

// System 是一个已装载的外部系统：定义、连接器与 webhook 签名密钥的位置。
type System struct {
	// Name 取外部系统定义的 name。
	Name string
	// Definition 是解析并校验过的外部系统定义。
	Definition Definition
	// DefinitionPath 是解析后的定义文件绝对路径或相对当前目录的规范路径。
	DefinitionPath string
	// SigningKeyFile 是解析后的 webhook 签名密钥文件路径，可以为空；
	// webhook 在每次请求时读取它，不可读时返回 503。
	SigningKeyFile string
	// APIBaseURL 是连接器调用状态查询的基地址。
	APIBaseURL string
	// Connector 是按定义名构造的连接器。
	Connector Connector
	// BindingValues 是该外部系统的项目绑定取值，已按定义规范化。
	BindingValues []string
}

type rawConfig struct {
	Project  string              `yaml:"project"`
	Systems  []rawSystemConfig   `yaml:"systems"`
	Bindings map[string][]string `yaml:"bindings"`
}

type rawSystemConfig struct {
	Definition     string `yaml:"definition"`
	SigningKeyFile string `yaml:"signing_key_file"`
	APIBaseURL     string `yaml:"api_base_url"`
}

// LoadConfig 读取并校验 connectorhub 配置，装载其中引用的全部外部系统定义。
//
// systems[].definition 与 systems[].signing_key_file 的相对路径按配置文件
// 所在目录解析，以 ~/ 开头的路径展开为当前用户的主目录。定义名没有对应
// 连接器、任一字段不合闭集或 v0.2.0 的取值校验不通过时返回错误。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("connectorhub: read config %s: %w", path, err)
	}

	var raw rawConfig
	if err := decodeStrictYAML(data, &raw); err != nil {
		return Config{}, fmt.Errorf("connectorhub: parse config %s: %w", path, err)
	}
	if raw.Project == "" {
		return Config{}, fmt.Errorf("connectorhub: config %s: project is required", path)
	}

	baseDir := filepath.Dir(path)
	config := Config{
		Project:  raw.Project,
		Bindings: make(map[string][]string, len(raw.Bindings)),
		byName:   make(map[string]int, len(raw.Systems)),
		byPath:   make(map[string]int),
	}

	for i, rawSystem := range raw.Systems {
		if rawSystem.Definition == "" {
			return Config{}, fmt.Errorf("connectorhub: config %s: systems[%d].definition is required", path, i)
		}

		definitionPath, err := resolveConfigPath(baseDir, rawSystem.Definition)
		if err != nil {
			return Config{}, fmt.Errorf("connectorhub: config %s: systems[%d].definition: %w", path, i, err)
		}
		definition, err := loadDefinition(definitionPath)
		if err != nil {
			return Config{}, fmt.Errorf("connectorhub: config %s: systems[%d]: %w", path, i, err)
		}

		if _, exists := config.byName[definition.Name]; exists {
			return Config{}, fmt.Errorf("connectorhub: config %s: duplicate system name %q", path, definition.Name)
		}

		connector, ok := newBuiltinConnector(definition.Name, rawSystem.APIBaseURL)
		if !ok {
			return Config{}, fmt.Errorf("connectorhub: config %s: no connector for system %q", path, definition.Name)
		}

		signingKeyFile, err := resolveConfigPath(baseDir, rawSystem.SigningKeyFile)
		if err != nil {
			return Config{}, fmt.Errorf("connectorhub: config %s: systems[%d].signing_key_file: %w", path, i, err)
		}

		config.Systems = append(config.Systems, System{
			Name:           definition.Name,
			Definition:     definition,
			DefinitionPath: definitionPath,
			SigningKeyFile: signingKeyFile,
			APIBaseURL:     rawSystem.APIBaseURL,
			Connector:      connector,
		})
		index := len(config.Systems) - 1
		config.byName[definition.Name] = index

		for _, source := range definition.Events.Sources {
			if source.Type != "webhook" {
				continue
			}
			if other, exists := config.byPath[source.Path]; exists {
				return Config{}, fmt.Errorf(
					"connectorhub: config %s: webhook path %q is used by both %q and %q",
					path, source.Path, config.Systems[other].Name, definition.Name)
			}
			config.byPath[source.Path] = index
		}
	}

	systemNames := make([]string, 0, len(raw.Bindings))
	for name := range raw.Bindings {
		systemNames = append(systemNames, name)
	}
	sort.Strings(systemNames)
	for _, name := range systemNames {
		index, ok := config.byName[name]
		if !ok {
			return Config{}, fmt.Errorf("connectorhub: config %s: bindings reference unknown system %q", path, name)
		}
		definition := config.Systems[index].Definition
		if definition.Events.ProjectBinding == "" {
			return Config{}, fmt.Errorf("connectorhub: config %s: system %q has no events.project_binding", path, name)
		}
		values := raw.Bindings[name]
		normalized := make([]string, len(values))
		for i, value := range values {
			normalized[i] = definition.Normalize(definition.Events.ProjectBinding, value)
		}
		config.Bindings[name] = normalized
		config.Systems[index].BindingValues = normalized
	}

	return config, nil
}

// System 按外部系统名查找已装载的系统。
func (c *Config) System(name string) (*System, bool) {
	if index, ok := c.byName[name]; ok {
		return &c.Systems[index], true
	}
	for i := range c.Systems {
		if c.Systems[i].Name == name {
			return &c.Systems[i], true
		}
	}
	return nil, false
}

// SystemByWebhookPath 按 webhook 接收路径查找已装载的系统。
func (c *Config) SystemByWebhookPath(path string) (*System, bool) {
	if index, ok := c.byPath[path]; ok {
		return &c.Systems[index], true
	}
	for i := range c.Systems {
		for _, source := range c.Systems[i].Definition.Events.Sources {
			if source.Type == "webhook" && source.Path == path {
				return &c.Systems[i], true
			}
		}
	}
	return nil, false
}

// ProjectsForBinding 返回某个项目归属键取值对应的项目。v0.2.0 只有一个
// 项目；取值未绑定时返回空。value 应已按定义规范化。
func (c *Config) ProjectsForBinding(system, value string) []string {
	for _, bound := range c.Bindings[system] {
		if bound == value {
			return []string{c.Project}
		}
	}
	return nil
}

// resolveConfigPath 按 LoadConfig 的规则解析配置中的路径：~/ 展开为主目录，
// 其余相对路径按配置文件所在目录解析，空路径保持为空。
func resolveConfigPath(baseDir, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %q: %w", path, err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(baseDir, path), nil
}

// decodeStrictYAML 把单个 YAML 文档解码到 out，并拒绝未知字段与额外文档。
func decodeStrictYAML(data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing YAML document")
		}
		return err
	}
	return nil
}
