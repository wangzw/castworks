# castworks

托管 AI Agent 环境：完整的第三方 Agent 运行在挂载持久化工作区的沙箱中，托管环境在 Agent 等待 LLM、外部系统或人类时挂起沙箱释放计算资源，并在依赖解除后恢复。

## Documentation

- design: docs/design
- requirements: docs/prd
- adr: docs/adr
- glossary: docs/glossary.md
- references: docs/references.md
- language: zh-CN

## Code Layout

- Each module's code lives in `pkg/` in a directory named after the module. The module table in `docs/design/README.md` is the authority; grouped modules use the group name (`warden`, not `driver-claude-code`; `sandboxprovider`, not `k8s`).
- A module's external protocol definitions and their generated code live in a version subdirectory of that module's directory, such as `pkg/agentprotocol/v1/`.
- `internal/` holds only test helpers that belong to no module.
- Executable programs live in `cmd/`.

## Commit Convention

Supplements the `commit-convention` skill.

### Scopes

- `architecture`: `docs/design/architecture.md`
- a module name: that module's documents. The module table in `docs/design/README.md` is the authority. Grouped modules use the group name: `warden`, not `driver-claude-code`; `sandboxprovider`, not `k8s`.
- `design`: cross-cutting changes
- `adr`: `docs/adr/`
- `glossary`: `docs/glossary.md`
- `references`: `docs/references.md`
- `milestone`: `docs/milestone/`
- `faq`: `docs/faq/`
- a module name for that module's code as well, located at `pkg/<module>`: `runcontroller`, `agentprotocol`, `warden`, `sandboxprovider`, `humanchannel`
- `controlplane`: `cmd/castworks-controlplane`
- `cli`: `cmd/castworks`
- `deploy`: `deploy/`
- `build`: `Makefile`, `go.mod`, lint and code generation configuration, `images/`
- `repo`: `CLAUDE.md`, `.markdownlint.jsonc` and other repository-level configuration

### Types

- `docs` is the default.
- Correcting a wrong, contradictory or broken statement, contract or check in a document is `fix`, not `docs`.
- `feat` and `perf` are not used until an implementation exists.

### Rules

- A change is breaking only when it removes or renames something another document or an implementation consumes: a protocol field, an RPC, a state name, an invariant, or a glossary term used by an interface.
