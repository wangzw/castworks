// Package acceptance holds the v0.1.0 real-environment acceptance tests.
//
// The tests drive the real castworks binaries against a PostgreSQL database,
// a local Claude Code or a Substrate cluster, and check the milestone's pass
// criteria. They sit outside the unit and integration suites behind the
// acceptance build tag, so neither `make test` nor `make test-integration`
// compiles them:
//
//	go vet -tags acceptance ./hack/acceptance/
//	go test -tags acceptance ./hack/acceptance/
//
// The tests are normally started through the Makefile targets, which check
// their required variables before running anything:
//
//	make acceptance-dryrun JOBS=<n>
//	make acceptance-local JOBS=<n> CREDENTIAL_FILE=<path>
//	make acceptance-substrate JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref>
//	make acceptance-suspend JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref>
//	make acceptance-ballast JOBS=<n> CREDENTIAL_FILE=<path> BALLAST_IMAGE=<ref>
//	make acceptance-clean JOBS=<n>
//
// Environment variables:
//
//   - CASTWORKS_TEST_DATABASE_URL: PostgreSQL server used to create one
//     database per test, as internal/testutil/pgtest expects.
//   - CASTWORKS_ACC_CREDENTIAL_FILE: file holding the LLM credential. It is
//     only ever passed to the control plane as --llm-credential-file; the
//     tests never read or log its contents.
//   - CASTWORKS_ACC_ACTOR_IMAGE: linux/arm64 manifest digest of the actor
//     image, in the name@sha256:<digest> form.
//   - CASTWORKS_ACC_ACTOR_COMMIT: the commit the actor image was built from.
//     The acceptance tests are committed after the image, so the branch HEAD
//     is not the image's provenance; the suspend measurement records this
//     value instead.
//   - CASTWORKS_ACC_BALLAST_IMAGE: linux/arm64 manifest digest of the
//     memory-ballast image, in the same form. acceptance-ballast sets it from
//     BALLAST_IMAGE, which defaults to ACTOR_IMAGE.
//   - CASTWORKS_ACC_CLAUDE_BIN: Claude Code executable installed by
//     install-claude.sh. The acceptance-local target produces it when it is
//     not given.
//   - CASTWORKS_ACC_KUBE_CONTEXT: optional kube context for kubectl ate.
//
// Every run works under /tmp/castworks-acceptance/<UTC timestamp>/ and never
// writes inside the repository. That root also holds the ledger, which
// records each created run and child process so acceptance-clean can remove
// what a killed test left behind.
package acceptance
