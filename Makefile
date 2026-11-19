GO ?= go
BUF ?= buf
BUF_VERSION ?= 1.75.0
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION ?= 2.15.0
COMPOSE ?= docker compose -f deploy/dev/compose.yaml

.PHONY: gen gen-check lint test gate test-integration db-up db-down check-jobs

# JOBS is the test concurrency passed to `go test -p`. It has no default:
# probing the CPU count or picking a number here would make runs depend on
# the machine. Targets that run tests refuse to start without it.
check-jobs:
	@if [ -z "$(JOBS)" ]; then \
		echo "JOBS is not set; run make $(MAKECMDGOALS) JOBS=<n>" >&2; \
		exit 1; \
	fi

# Code generation needs buf.gen.yaml. The skeleton commit predates it; there
# gen and gen-check print why they skip instead of failing, so `make gate`
# works on the skeleton checked out alone. The condition is on the make side
# because a recipe line's `exit` only ends that line's shell, not the target.
ifeq ($(wildcard buf.gen.yaml),)
gen:
	@echo "gen: buf.gen.yaml is absent; skipping code generation (this commit predates the protocol definitions)" >&2

gen-check:
	@echo "gen-check: buf.gen.yaml is absent; skipping code generation consistency check (this commit predates the protocol definitions)" >&2
else
gen:
	@if [ "$$($(BUF) --version)" != "$(BUF_VERSION)" ]; then \
		echo "buf $(BUF_VERSION) is required; found:" >&2; \
		$(BUF) --version >&2; \
		exit 1; \
	fi
	$(BUF) generate

# gen-check generates into a fresh temporary directory and compares that output
# with the committed *.pb.go files under api. It never regenerates in place, so
# a generation failure or a mismatch leaves the committed files untouched and
# the module still compiles. The comparison runs in both directions: every
# generated file must be committed and identical, and every committed *.pb.go
# must still be produced, so a file left behind by a deleted or renamed proto
# cannot survive and be missed.
gen-check:
	@if [ "$$($(BUF) --version)" != "$(BUF_VERSION)" ]; then \
		echo "buf $(BUF_VERSION) is required; found:" >&2; \
		$(BUF) --version >&2; \
		exit 1; \
	fi; \
	tmp="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmp"' EXIT INT TERM HUP; \
	if ! $(BUF) generate --output "$$tmp"; then \
		echo "gen-check: buf generate failed; the committed files under api are unchanged" >&2; \
		exit 1; \
	fi; \
	status=0; \
	for generated in $$(find "$$tmp/api" -name '*.pb.go' -type f); do \
		rel="$${generated#$$tmp/api/}"; \
		if [ ! -f "api/$$rel" ]; then \
			echo "generated but not committed: api/$$rel" >&2; \
			status=1; \
		elif ! cmp -s "$$generated" "api/$$rel"; then \
			echo "generated output differs: api/$$rel" >&2; \
			status=1; \
		fi; \
	done; \
	for committed in $$(find api -name '*.pb.go' -type f); do \
		rel="$${committed#api/}"; \
		if [ ! -f "$$tmp/api/$$rel" ]; then \
			echo "committed but no longer generated: api/$$rel" >&2; \
			status=1; \
		fi; \
	done; \
	if [ "$$status" -ne 0 ]; then \
		echo "gen-check: generated files differ from the committed ones" >&2; \
		exit 1; \
	fi; \
	echo "gen-check: generated files match the committed ones"
endif

lint:
	@$(GOLANGCI_LINT) --version | grep -q 'version $(GOLANGCI_LINT_VERSION) ' || { \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) is required; found:" >&2; \
		$(GOLANGCI_LINT) --version >&2; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run ./...

test: check-jobs
	$(GO) test -p $(JOBS) -parallel $(JOBS) ./...

gate: check-jobs gen-check lint test

# test-integration uses the integration build tag, which is added on top of the
# unit tests rather than replacing them: `go test -tags integration` runs both,
# so it needs the test database (make db-up) as well as the cluster.
test-integration: check-jobs
	$(GO) test -tags integration -p $(JOBS) -parallel $(JOBS) ./...

db-up:
	$(COMPOSE) up -d --wait postgres

db-down:
	$(COMPOSE) down

# actor 镜像只构建 linux/arm64。actor-image 只打本地标签，供镜像冒烟使用；
# actor-image-push 在干净的工作树上推送按提交短 SHA 命名的镜像，标准输出
# 最后一行为 <仓库>@sha256:<arm64 镜像清单 digest>，不含多平台索引。
ACTOR_IMAGE ?= castworks/actor:dev
ACTOR_IMAGE_REPO ?= registry.zhanwei.wang/castworks/actor

.PHONY: actor-image actor-image-push

actor-image:
	images/actor/build.sh --tag $(ACTOR_IMAGE)

actor-image-push:
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "actor-image-push: 工作树不干净，提交或清理后再推送" >&2; \
		git status --porcelain >&2; \
		exit 1; \
	fi
	images/actor/build.sh --push --tag $(ACTOR_IMAGE_REPO):$$(git rev-parse --short HEAD)

# actor-image-smoke 在本地镜像上验证 Claude Code 版本、actor 模式的就绪探针、
# PID 1 与运行用户、关停耗时和退出码，以及非法 actor-name 的退出状态。
.PHONY: actor-image-smoke

actor-image-smoke: actor-image
	images/actor/smoke.sh --image $(ACTOR_IMAGE)
