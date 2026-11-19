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
	$(GO) test -race -p $(JOBS) -parallel $(JOBS) ./...

gate: check-jobs gen-check lint test

# test-integration uses the integration build tag, which is added on top of the
# unit tests rather than replacing them: `go test -tags integration` runs both,
# so it needs the test database (make db-up) as well as the cluster.
test-integration: check-jobs
	$(GO) test -tags integration -p $(JOBS) -parallel $(JOBS) ./...

# 测试连 postgres-test，开发机上运行的控制面连 postgres。两者用途的区别见
# deploy/dev/README.md。测试读取的连接串示例：
#   postgres://castworks:castworks@127.0.0.1:55433/castworks_test?sslmode=disable
db-up:
	$(COMPOSE) up -d --wait postgres postgres-test

db-down:
	$(COMPOSE) down

# actor 镜像只构建 linux/arm64。actor-image 只打本地标签，供镜像冒烟使用；
# actor-image-push 在干净的工作树上推送按提交短 SHA 命名的镜像，标准输出
# 最后一行为 <仓库>@sha256:<arm64 镜像清单 digest>，不含多平台索引。
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

# Real-environment acceptance. These targets stay outside test-integration:
# they need the developer machine, an LLM credential, and for the substrate
# steps a cluster. Run them in order, and only after the upstream units are
# merged. The tests themselves carry the acceptance build tag, so make test
# and make test-integration never compile them.

# The image names and paths come from the command line: none of them has a
# default here. ACTOR_IMAGE is the actor image under test and ACTOR_COMMIT is
# the commit that image was built from; a run that records provenance refuses
# to start without the latter. Each target checks the values it needs before
# starting anything.
# BALLAST_IMAGE is the image the memory-ballast test provisions. It is
# required on its own and does not default to ACTOR_IMAGE.
CREDENTIAL_FILE ?=
CLAUDE_BIN ?=
KUBE_CONTEXT ?=
RUN ?=

.PHONY: acceptance-dryrun acceptance-local acceptance-substrate acceptance-suspend acceptance-ballast acceptance-clean check-database-url

ACCEPTANCE_ENV = CASTWORKS_ACC_CREDENTIAL_FILE="$(CREDENTIAL_FILE)" CASTWORKS_ACC_ACTOR_IMAGE="$(ACTOR_IMAGE)" CASTWORKS_ACC_ACTOR_COMMIT="$(ACTOR_COMMIT)" CASTWORKS_ACC_CLAUDE_BIN="$(CLAUDE_BIN)" CASTWORKS_ACC_KUBE_CONTEXT="$(KUBE_CONTEXT)"

# -run takes a Go regexp; "|" is alternation. It is not grep's "\|". Every
# alternative is anchored at both ends so a test whose name starts with
# another name is not selected.
check-database-url:
	@if [ -z "$$CASTWORKS_TEST_DATABASE_URL" ]; then \
		echo "CASTWORKS_TEST_DATABASE_URL is not set; run make db-up and export it" >&2; \
		exit 1; \
	fi

# The acceptance test targets below run go test through `tee` and keep its exit
# status in a file. A "-run" pattern that matches no test makes go test exit 0
# with only a warning, which would report a green acceptance run that never
# ran; the recipe fails unless the verbose output contains at least one
# "=== RUN" line. The subshell records the status because the pipeline's own
# status would be tee's, and /bin/sh has no PIPESTATUS. Each target keeps its
# full verbose output under /tmp/castworks-acceptance/ with a UTC timestamp in
# the name, so a finished run can be read after the fact and consecutive runs
# do not overwrite each other.

acceptance-dryrun: check-jobs check-database-url
	@mkdir -p /tmp/castworks-acceptance; \
	log=/tmp/castworks-acceptance/acceptance-dryrun-$$(date -u +%Y%m%dT%H%M%SZ).log; code=$$(mktemp); \
	trap 'rm -f "$$code"' EXIT INT TERM HUP; \
	( $(ACCEPTANCE_ENV) $(GO) test -tags acceptance -count=1 -v -p $(JOBS) -parallel $(JOBS) -timeout 10m -run '^TestDryRun$$|^TestSnapshotSizeUnit$$|^TestBallastStatsUnit$$|^TestChildProcessIsolation$$|^TestCleanupSavesActorBeforeEnd$$|^TestStopRecordedProcessMatchesExecutablePath$$|^TestWaitLoggedAddrReadsEveryLog$$|^TestDoSetsJSONContentTypeOnBodylessPost$$|^TestFakeClaudeEmitsSessionStateEvents$$|^TestSuspendParsesReplies$$|^TestSuspendFileProof$$|^TestSuspendProvenanceUsesActorCommit$$|^TestSuspendAllocatedComparison$$|^TestSuspendRunStateWatcher$$|^TestMeasureSnapshotTracker$$' ./hack/acceptance/; echo $$? > "$$code" ) | tee "$$log"; \
	status=$$(cat "$$code"); \
	if [ "$$status" -eq 0 ] && ! grep -q '^=== RUN' "$$log"; then \
		echo "acceptance-dryrun: the -run pattern matched no test; failing instead of reporting success" >&2; \
		exit 1; \
	fi; \
	exit "$$status"

acceptance-local: check-jobs check-database-url
	@if [ -z "$(CREDENTIAL_FILE)" ]; then \
		echo "CREDENTIAL_FILE is not set; run make acceptance-local JOBS=<n> CREDENTIAL_FILE=<path>" >&2; \
		exit 1; \
	fi
	@set -e; \
	mkdir -p /tmp/castworks-acceptance; \
	claude_bin="$(CLAUDE_BIN)"; \
	if [ -z "$$claude_bin" ]; then \
		claude_bin="$$(./hack/acceptance/install-claude.sh)"; \
	fi; \
	log=/tmp/castworks-acceptance/acceptance-local-$$(date -u +%Y%m%dT%H%M%SZ).log; code=$$(mktemp); \
	trap 'rm -f "$$code"' EXIT INT TERM HUP; \
	( CASTWORKS_ACC_CREDENTIAL_FILE="$(CREDENTIAL_FILE)" \
		CASTWORKS_ACC_ACTOR_IMAGE="$(ACTOR_IMAGE)" \
		CASTWORKS_ACC_CLAUDE_BIN="$$claude_bin" \
		CASTWORKS_ACC_KUBE_CONTEXT="$(KUBE_CONTEXT)" \
		$(GO) test -tags acceptance -count=1 -v -p $(JOBS) -parallel $(JOBS) -timeout 30m -run '^TestLocal$$' ./hack/acceptance/; echo $$? > "$$code" ) | tee "$$log"; \
	status=$$(cat "$$code"); \
	if [ "$$status" -eq 0 ] && ! grep -q '^=== RUN' "$$log"; then \
		echo "acceptance-local: the -run pattern matched no test; failing instead of reporting success" >&2; \
		exit 1; \
	fi; \
	exit "$$status"

acceptance-substrate: check-jobs check-database-url
	@if [ -z "$(CREDENTIAL_FILE)" ]; then \
		echo "CREDENTIAL_FILE is not set; run make acceptance-substrate JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref>" >&2; \
		exit 1; \
	fi
	@if [ -z "$(ACTOR_IMAGE)" ]; then \
		echo "ACTOR_IMAGE is not set; run make acceptance-substrate JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref>" >&2; \
		exit 1; \
	fi
	@mkdir -p /tmp/castworks-acceptance; \
	log=/tmp/castworks-acceptance/acceptance-substrate-$$(date -u +%Y%m%dT%H%M%SZ).log; code=$$(mktemp); \
	trap 'rm -f "$$code"' EXIT INT TERM HUP; \
	( $(ACCEPTANCE_ENV) $(GO) test -tags acceptance -count=1 -v -p $(JOBS) -parallel $(JOBS) -timeout 30m -run '^TestSubstrateTwoTurns$$' ./hack/acceptance/; echo $$? > "$$code" ) | tee "$$log"; \
	status=$$(cat "$$code"); \
	if [ "$$status" -eq 0 ] && ! grep -q '^=== RUN' "$$log"; then \
		echo "acceptance-substrate: the -run pattern matched no test; failing instead of reporting success" >&2; \
		exit 1; \
	fi; \
	exit "$$status"

acceptance-suspend: check-jobs check-database-url
	@if [ -z "$(CREDENTIAL_FILE)" ]; then \
		echo "CREDENTIAL_FILE is not set; run make acceptance-suspend JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref> ACTOR_COMMIT=<sha>" >&2; \
		exit 1; \
	fi
	@if [ -z "$(ACTOR_IMAGE)" ]; then \
		echo "ACTOR_IMAGE is not set; run make acceptance-suspend JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref> ACTOR_COMMIT=<sha>" >&2; \
		exit 1; \
	fi
	@if [ -z "$(ACTOR_COMMIT)" ]; then \
		echo "ACTOR_COMMIT is not set; run make acceptance-suspend JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref> ACTOR_COMMIT=<sha>" >&2; \
		exit 1; \
	fi
	@if [ -z "$(RUN)" ]; then run='^TestSuspendResume$$'; else run='^TestSuspendResume$$/^$(RUN)$$'; fi; \
	mkdir -p /tmp/castworks-acceptance; \
	log=/tmp/castworks-acceptance/acceptance-suspend-$$(date -u +%Y%m%dT%H%M%SZ).log; code=$$(mktemp); \
	trap 'rm -f "$$code"' EXIT INT TERM HUP; \
	( $(ACCEPTANCE_ENV) $(GO) test -tags acceptance -count=1 -v -p $(JOBS) -parallel $(JOBS) -timeout 90m -run "$$run" ./hack/acceptance/; echo $$? > "$$code" ) | tee "$$log"; \
	status=$$(cat "$$code"); \
	if [ "$$status" -eq 0 ] && ! grep -q '^=== RUN' "$$log"; then \
		echo "acceptance-suspend: the -run pattern matched no test; failing instead of reporting success" >&2; \
		exit 1; \
	fi; \
	exit "$$status"

# acceptance-ballast provisions the memory-ballast actor and runs five
# snapshot/restore cycles. It starts no control plane and spends no LLM quota,
# but it serves actor reports on the development host's 9090, the same address
# a control plane uses, so it must not overlap with one. The test context
# expires at 45m; -timeout 60m lets a deadline fail the test and run its
# cleanup instead of letting go test panic. It needs no credential file; its
# image comes from BALLAST_IMAGE, which has no default.
acceptance-ballast: check-jobs check-database-url
	@if [ -z "$(BALLAST_IMAGE)" ]; then \
		echo "BALLAST_IMAGE is not set; run make acceptance-ballast JOBS=<n> BALLAST_IMAGE=<ref>" >&2; \
		exit 1; \
	fi
	@mkdir -p /tmp/castworks-acceptance; \
	log=/tmp/castworks-acceptance/acceptance-ballast-$$(date -u +%Y%m%dT%H%M%SZ).log; code=$$(mktemp); \
	trap 'rm -f "$$code"' EXIT INT TERM HUP; \
	( $(ACCEPTANCE_ENV) CASTWORKS_ACC_BALLAST_IMAGE="$(BALLAST_IMAGE)" $(GO) test -tags acceptance -count=1 -v -p $(JOBS) -parallel $(JOBS) -timeout 60m -run '^TestBallastSuspendResume$$' ./hack/acceptance/; echo $$? > "$$code" ) | tee "$$log"; \
	status=$$(cat "$$code"); \
	if [ "$$status" -eq 0 ] && ! grep -q '^=== RUN' "$$log"; then \
		echo "acceptance-ballast: the -run pattern matched no test; failing instead of reporting success" >&2; \
		exit 1; \
	fi; \
	exit "$$status"

acceptance-clean: check-jobs
	@mkdir -p /tmp/castworks-acceptance; \
	log=/tmp/castworks-acceptance/acceptance-clean-$$(date -u +%Y%m%dT%H%M%SZ).log; code=$$(mktemp); \
	trap 'rm -f "$$code"' EXIT INT TERM HUP; \
	( $(ACCEPTANCE_ENV) $(GO) test -tags acceptance -count=1 -v -p $(JOBS) -parallel $(JOBS) -timeout 10m -run '^TestClean$$' ./hack/acceptance/; echo $$? > "$$code" ) | tee "$$log"; \
	status=$$(cat "$$code"); \
	if [ "$$status" -eq 0 ] && ! grep -q '^=== RUN' "$$log"; then \
		echo "acceptance-clean: the -run pattern matched no test; failing instead of reporting success" >&2; \
		exit 1; \
	fi; \
	exit "$$status"
