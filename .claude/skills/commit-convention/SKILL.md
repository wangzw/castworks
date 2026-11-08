---
name: commit-convention
description: castworks Commit Message Convention — English Conventional Commits, scopes taken from the documentation tree, and repository-specific rules
triggers:
  - creating a commit
  - writing a commit message
  - git commit
  - committing changes
globs:
  - "**/*"
---

# castworks Commit Message Convention

Follows [Conventional Commits](https://www.conventionalcommits.org/); the convention is enforced in review — there is no commitlint config or commit-msg hook in this repository.

**This repository is a documentation repository today.** It holds design documents, ADRs, a glossary, a references file, a roadmap and an FAQ — no Go module, npm package, OpenAPI file or release process. The scopes below come from that tree. When implementation lands, extend this convention from the new directory structure; do not import another project's scope list.

**Language: English, always.** The documents are written in Chinese; commit messages are the one place the two languages part. Module names, protocol fields, ADR and REF identifiers, file paths and the English counterparts registered in the glossary (`docs/glossary.md`) keep their exact spelling. When a change concerns a registered term, use the term's English counterpart rather than a fresh translation, and do not put Chinese prose in the message.

## Format

```text
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

- **Header** is required and must be a single line.
- **Keep the header short — aim for 72 characters or less.** This is a guideline, not a hard limit: a header that needs a few more characters to stay clear is better than a truncated one. Do not sacrifice meaning for the count.
- **Scope is required.** Every change belongs to a named part of the documentation tree; without a scope the header cannot say which one.
- **Description** uses the imperative mood, lowercase, no trailing period: "order approval deadlines before the driver cutoff", not "ordered ..." or "orders ...".
- **Body** states the problem, the decision and its cost. For a substantive change it is expected, not optional: the reader of `git log` has the diff and needs the reason. A change confined to one obvious line may omit it.
- **Footer** carries `BREAKING CHANGE:` notices and references to durable external identifiers such as public issue numbers. ADR and REF identifiers belong in the body at the sentence that relies on them, not in the footer.

## Types

| Type | When to use |
| --- | --- |
| `docs` | Documentation content: design documents, ADRs, glossary, references, roadmap, FAQ. The default in this repository. |
| `fix` | Correcting a wrong, contradictory or broken statement, contract or check. |
| `refactor` | Restructuring documents without changing what they assert: moving sections, splitting or merging files. |
| `style` | Formatting, punctuation or link syntax only; no change in what a document asserts. |
| `test` | Adding or updating checks or tests for the documentation tooling. |
| `chore` | Repository configuration, tooling, skills and dependencies. |
| `feat` | New capability or behaviour in code. A new design document is `docs`; this repository has no implementation for `feat` to describe yet. |
| `perf` | Performance improvement in code; not applicable until an implementation exists. |

## Scopes

Scope names the part of the tree the change belongs to. The module table in `docs/design/README.md` is the membership authority for module scopes; read that table rather than relying on a list here, which would go stale. Grouped modules commit under the group scope, never the submodule: `warden`, not `driver-claude-code`; `sandboxprovider`, not `k8s`.

### Design documents

- `architecture` — `docs/design/architecture.md`
- the module name — e.g. `runcontroller`, `warden`, `suspendpolicy`, `connectorhub`; the change is confined to that module's documents
- `design` — a change that spans several design documents and belongs to no single module, such as a cross-document contract alignment

### Shared documents

- `adr` — `docs/adr/`
- `glossary` — `docs/glossary.md`
- `references` — `docs/references.md`
- `roadmap` — `docs/roadmap.md`
- `faq` — `docs/faq/`

### Repository

- `repo` — `CLAUDE.md`, `.claude/skills/`, `.markdownlint.jsonc` and other repository-level configuration

**One scope per commit.** When a design change also updates the glossary and the roadmap for consistency, the scope stays the module or `design`; incidental consistency edits elsewhere do not get a scope of their own.

## Examples

```text
docs(design): order approval deadlines and tolerate event write lag

fix(suspendpolicy): exclude loopback connections from the inbound wake check

docs(adr): record wait intent as a soft input

docs(glossary): register the effective self-wake entry

refactor(design): split idle detection into freezable and destroyable criteria

style(roadmap): escape table pipes and wrap bare URLs in links

chore(repo): add the commit message convention skill

docs(agentprotocol)!: remove request_hash from the protocol event

BREAKING CHANGE: request_hash is gone from the event schema; a consumer that
keyed on it must use request_id. The event catalogue and the module documents
are updated in this commit.
```

## Rules

1. **One logical change per commit.** A single design change routinely spans the architecture document, one or more module documents, an ADR, the glossary and the roadmap. That is one logical change; the file count does not make it several. Do not bundle unrelated design changes.
2. **Each commit leaves the document set consistent.** Every document and index the change makes stale is updated in the same commit: a term rename reaches the glossary, the architecture document, every module document, the ADR records, the roadmap and the FAQ. A leftover occurrence is a defect, not a follow-up.
3. **No generic messages.** "update docs", "fix typo" and "misc changes" are rejected.
4. **Breaking changes** get `!` after the scope and a `BREAKING CHANGE:` footer. A documented contract is breaking when a change removes or renames something another document or an implementation consumes: a protocol field, an RPC, a state name, an invariant, or a glossary term used by an interface. The commit updates the documents that define the contract.
5. **A decision belongs to an ADR; the commit says what changed.** When a change adopts, replaces or reverses a design decision, add or revise the ADR in the same commit. The commit body states the decision and its cost, not the debate that produced it.
6. **Verify before committing.** Run the documentation structural checker from the repository root and leave it at exit 0:

   ```bash
   python3 ~/.claude/skills/document-standards/scripts/check_documents.py
   ```

   Run markdownlint against the repository configuration, and render every diagram in a changed document:

   ```bash
   markdownlint-cli2 'docs/**/*.md' 'CLAUDE.md'
   ```

   A Mermaid diagram that fails to parse is invisible in a text diff and must not ship; render each changed diagram with `mmdc` and confirm it succeeds.
7. **A `fix` commit carries the check that reproduces the defect.** This repository has no code or test suite. For a document defect, state in the body what was wrong and how the correction was verified; where the defect is mechanically checkable, extend the document-standards planted-violation suite in `scripts/test_check_documents.py` and run it. Once implementation and tests exist, every bug fix carries a test that fails before the fix and passes after it.
8. **Keep `main` linear.** Rebase the feature branch onto `main`, then merge with `git merge --ff-only`; never create a merge commit.
9. **Do not rewrite commits already on `main`.** A mistake in a published commit is corrected by a new commit, not by rebasing `main`.

## Content constraints

The subject and body say what this change decides and why it was needed, for the person who will dig through `git log` later. They do not narrate the process that produced the change.

1. **No internal process references anywhere** — subject, body or footer. No finding IDs (`P3-…`, "reviewer finding …"), no round names ("the third review round"), no internal plan or verification states, and no document filename cited as authority. Naming a file the commit itself changes is describing the diff, not a citation. The footer's reference genre is for durable external identifiers such as public issue numbers only.
2. **The body is a concluding why**: the problem, the decision and its cost. No module biography, no process history (review rounds, verification states, follow-up markers). One sentence of "before this change" context is fine.
3. **Numbers that describe this diff** ("from four to six") are fine — they are checkable against the diff. Numbers describing the current state outside this diff ("N modules now …") are not.
4. These constraints bind future commits only — apart from the one-time migration of the existing history to English, commits already on `main` are never rewritten.

Good body (concluding why):

```text
fix(suspendpolicy): exclude loopback connections from the inbound check

The inbound-wake check treated any queued byte as external input. A connection
whose both ends sit inside the agent process tree can queue bytes with no
external sender, so the check could keep a frozen sandbox alive indefinitely.
The check now skips loopback connections whose peer socket belongs to the same
process tree.
```

Bad body (process narration and artifacts):

```text
fix(suspendpolicy): address reviewer finding P3-wake

The third review round flagged that the inbound check counted loopback bytes
(the P3-wake finding, from the wake-hardening round), and after a
NEEDS_FOLLOWUP discussion the implementation decided to exclude loopback
connections, closing the finding per the agreed disposition in the review
ledger.
```
