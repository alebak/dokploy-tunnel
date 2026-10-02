# Skill Registry — dokploy-tunnel

Generated: 2026-10-01. Index only; each SKILL.md is the source of truth. Pass exact paths to subagents under '## Skills to load before work'.

Sources scanned (in order): ./skills, ./.agents/skills, ~/.claude/skills, ~/.agents/skills, ~/.config/opencode/skills

| Skill | Trigger / description | Scope | Path |
| --- | --- | --- | --- |
| branch-pr | Create Gentle AI pull requests with issue-first checks. Trigger: creating, opening, or preparing PRs for review. | user | /home/alebak/.claude/skills/branch-pr/SKILL.md |
| chained-pr | Trigger: PRs over 400 lines, stacked PRs, review slices. Split oversized changes into chained PRs that protect review focus. | user | /home/alebak/.claude/skills/chained-pr/SKILL.md |
| cognitive-doc-design | Design docs that reduce cognitive load. Trigger: writing guides, READMEs, RFCs, onboarding, architecture, or review-facing docs. | user | /home/alebak/.claude/skills/cognitive-doc-design/SKILL.md |
| comment-writer | Write warm, direct collaboration comments. Trigger: PR feedback, issue replies, reviews, Slack messages, or GitHub comments. | user | /home/alebak/.claude/skills/comment-writer/SKILL.md |
| diagnose-crash | > | user | /usr/share/omarchy/default/agents/skills/diagnose-crash/SKILL.md |
| gentle-ai-bench | Trigger: bench, journey, journeys, driven mode, gentle-ai-bench, journey corpus, j-numbers, bench axis. Author and verify gentle-ai bench journeys; go test ./bench never proves dri | user | /home/alebak/.claude/skills/gentle-ai-bench/SKILL.md |
| go-testing | Trigger: Go tests, go test coverage, Bubbletea teatest, golden files. Apply focused Go testing patterns. | user | /home/alebak/.claude/skills/go-testing/SKILL.md |
| grilling | Grill the user relentlessly about a plan, decision, or idea. Use when the user wants to stress-test their thinking, or uses any 'grill' trigger phrases. | user | /home/alebak/.claude/skills/grilling/SKILL.md |
| grill-me | Trigger: grill me, grillame, interrogame, stress-test this plan. A relentless interview that sharpens a plan or design before it becomes a PRD. | user | /home/alebak/.claude/skills/grill-me/SKILL.md |
| handoff | Trigger: handoff, hand this off, pass this to another session, entregar contexto. Compact the current conversation into a handoff document a fresh agent can pick up. | user | /home/alebak/.claude/skills/handoff/SKILL.md |
| herdr | Control Herdr, a terminal multiplexer for coding agents. Use only when the user explicitly mentions Herdr or asks to use Herdr to inspect or control panes, tabs, workspaces, comman | user | /home/alebak/.agents/skills/herdr/SKILL.md |
| issue-creation | Trigger: issue creation, bug reports, feature requests, or issue approval. Create and triage GitHub issues from repository evidence. | user | /home/alebak/.claude/skills/issue-creation/SKILL.md |
| judgment-day | Trigger: judgment day, dual review, adversarial review, juzgar. Run explicit blind dual review with at most two scoped fix/re-judgment rounds. | user | /home/alebak/.claude/skills/judgment-day/SKILL.md |
| omarchy | > | user | /usr/share/omarchy/default/agents/skills/omarchy/SKILL.md |
| prototype | Build a throwaway prototype to answer a design question. Use when the user wants to sanity-check whether a state model or logic feels right, or explore what a UI should look like. | user | /home/alebak/.claude/skills/prototype/SKILL.md |
| rdd-defect-workflow | Trigger: RDD, receipt-driven development, review authority, receipt/lineage, correction/recovery, delivery gate/kill switch, bounded review defects. Guide work. | user | /home/alebak/.claude/skills/rdd-defect-workflow/SKILL.md |
| requirements-archaeology | Trigger: PRD for legacy product, reverse-engineer requirements, document a system in production with no faithful PRD. Rebuild a verifiable PRD from manuals, code and probes. | user | /home/alebak/.claude/skills/requirements-archaeology/SKILL.md |
| skill-creator | Trigger: new skills, agent instructions, documenting AI usage patterns. Create LLM-first skills with valid frontmatter. | user | /home/alebak/.claude/skills/skill-creator/SKILL.md |
| skill-improver | Trigger: improve skills, audit skills, refactor skills, skill quality. Audit and upgrade existing LLM-first skills. | user | /home/alebak/.claude/skills/skill-improver/SKILL.md |
| systemic-issue-triage | Trigger: new issue, bug report, triage, backlog, issue flood, community report, root cause, dead-end, blocked user. Attack issues by root class, never one-by-one; fixes must shrink | user | /home/alebak/.claude/skills/systemic-issue-triage/SKILL.md |
| to-questionnaire | Turn a decision you can't fully answer into a questionnaire for someone else to fill in. | user | /home/alebak/.claude/skills/to-questionnaire/SKILL.md |
| work-unit-commits | Plan commits as reviewable work units. Trigger: implementation, commit splitting, chained PRs, or keeping tests and docs with code. | user | /home/alebak/.claude/skills/work-unit-commits/SKILL.md |
| computer-use | >- | user | /home/alebak/.agents/skills/computer-use/SKILL.md |
| find-skills | Helps users discover and install agent skills when they ask questions like how do I do X, find a skill for X, is there a skill that can..., or express interest in extending capabil | user | /home/alebak/.agents/skills/find-skills/SKILL.md |
| orca-cli | >- | user | /home/alebak/.agents/skills/orca-cli/SKILL.md |
| orchestration | >- | user | /home/alebak/.agents/skills/orchestration/SKILL.md |
