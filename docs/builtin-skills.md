# Builtin Skills

Builtin skills are system-level, operator-managed skills that are available to all agents but cannot be installed, removed, or overwritten by the agent. They are the right primitive for reusable, on-demand procedures that your backend controls.

## How skills work in PicoClaw

PicoClaw resolves skills from three tiers, in priority order:

| Tier | Location | Who controls it | Agent can modify? |
|---|---|---|---|
| workspace | `{workspace}/skills/{name}/SKILL.md` | User | Yes — via `install_skill` tool |
| global | `~/.picoclaw/skills/{name}/SKILL.md` | User | Yes |
| builtin | `$PICOCLAW_BUILTIN_SKILLS/{name}/SKILL.md` | Operator | **No** |

When the agent requests a skill by name, the first match wins. If a workspace skill has the same name as a builtin, the workspace skill shadows the builtin.

The agent cannot install or remove skills from the builtin path. Only `{workspace}/skills/` is writable by the `install_skill` tool.

## Configuring the builtin skills path

Set the environment variable `PICOCLAW_BUILTIN_SKILLS` to the directory that contains your system skills:

```bash
export PICOCLAW_BUILTIN_SKILLS=/etc/picoclaw/skills
```

If the variable is not set, PicoClaw defaults to `{current working directory}/skills` — the `skills/` directory next to the binary. In production deployments you should always set this explicitly.

### Docker / Kubernetes

Mount your skills directory read-only and set the env var:

```yaml
# docker-compose
services:
  picoclaw:
    image: your-picoclaw-image
    environment:
      PICOCLAW_BUILTIN_SKILLS: /etc/picoclaw/skills
    volumes:
      - ./system-skills:/etc/picoclaw/skills:ro
```

```yaml
# Kubernetes Deployment
env:
  - name: PICOCLAW_BUILTIN_SKILLS
    value: /etc/picoclaw/skills
volumeMounts:
  - name: system-skills
    mountPath: /etc/picoclaw/skills
    readOnly: true
volumes:
  - name: system-skills
    configMap:
      name: picoclaw-system-skills
```

## Skill file format

Each skill lives in its own subdirectory. The directory name is used as a fallback skill name if frontmatter is absent.

```
/etc/picoclaw/skills/
  git-workflow/
    SKILL.md
  code-review/
    SKILL.md
  deploy/
    SKILL.md
    scripts/
      pre-deploy-check.sh
```

### `SKILL.md` structure

```markdown
---
name: git-workflow
description: "Standard git branching and commit workflow for this org."
---

# Git Workflow

Always create a feature branch from `main`:

```bash
git checkout main && git pull
git checkout -b feature/your-feature-name
```

Commit messages must follow Conventional Commits:
- `feat:` new feature
- `fix:` bug fix
- `chore:` maintenance

Before opening a PR, squash commits and run the test suite.
```

**Frontmatter fields:**

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Unique skill name (alphanumeric + hyphens, max 64 chars) |
| `description` | Yes | Short description shown in the agent's skill list (max 1024 chars) |

The `name` field must match `^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`. If a workspace skill has the same `name`, it will shadow this builtin.

## How the agent uses skills

The agent sees a `<skills>` block in its system prompt listing all available skills by name and description. The agent decides when to load a skill's full content — it is not injected unconditionally.

```xml
<skills>
  <skill>
    <name>git-workflow</name>
    <description>Standard git branching and commit workflow for this org.</description>
    <location>/etc/picoclaw/skills/git-workflow/SKILL.md</location>
    <source>builtin</source>
  </skill>
</skills>
```

When the agent needs to use the skill, it requests the full content, which is then injected into context.

## Comparison: builtin skills vs `system_files_path`

| | Builtin skills | `system_files_path` |
|---|---|---|
| Content always in context | No — loaded on demand | Yes — always injected |
| Agent can choose to ignore | Yes | No |
| Format required | `SKILL.md` + frontmatter | Any `.md` file |
| Best for | Reusable procedures the agent invokes when relevant | Standing rules and policies that must always apply |

Use builtin skills for **reusable procedures** (git workflows, deploy scripts, code review checklists) that the agent invokes when the task calls for it.

Use `system_files_path` for **mandatory standing instructions** (compliance rules, safety policies, org-wide constraints) that must be present in every conversation regardless of task type.

## Practical example

```
/etc/picoclaw/skills/
  git-workflow/
    SKILL.md          # git branching rules
  code-review/
    SKILL.md          # PR checklist
  docker-deploy/
    SKILL.md          # deploy procedure
    scripts/
      health-check.sh
```

`git-workflow/SKILL.md`:

```markdown
---
name: git-workflow
description: "Git branching, commit, and PR conventions for this organisation."
---

# Git Workflow

## Branch naming
- Features: `feature/<ticket>-short-description`
- Fixes: `fix/<ticket>-short-description`

## Commit messages
Follow Conventional Commits. Run `git commit --no-verify` is forbidden.

## PR process
1. Open draft PR early.
2. Assign at least one reviewer.
3. Squash merge only.
```

Start PicoClaw with the builtin skills directory:

```bash
PICOCLAW_BUILTIN_SKILLS=/etc/picoclaw/skills picoclaw start
```

The agent will list `git-workflow`, `code-review`, and `docker-deploy` in its skill menu and load whichever it needs for the task at hand.
