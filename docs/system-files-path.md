# System Files Path

PicoClaw supports a separate directory of **system-provided instruction files** that are loaded as immutable additional context in the agent's system prompt. This is designed for multi-tenant deployments (e.g. Kubernetes pods) where a backend needs to inject platform-level behaviour, policies, or restrictions that users and agents cannot modify.

---

## What It Is

When `system_files_path` is configured, PicoClaw recursively discovers all `.md` files in that directory and injects their content into the system prompt under a `# System Instructions` section. These files:

- Are **additive** — they do not replace user-editable bootstrap files (`AGENTS.md`, `SOUL.md`, `USER.md`, `IDENTITY.md`)
- Are **read by the agent** but cannot be written to at runtime (enforced by the OS-level read-only mount in K8s deployments)
- Are **freely updatable** by the backend without touching the user's workspace
- Are **automatically reloaded** when changed — the context cache invalidates on any mtime change

---

## Configuration

### JSON config (`config.json`)

```json
{
  "agents": {
    "defaults": {
      "system_files_path": "/etc/picoclaw"
    }
  }
}
```

### Environment variable

```
PICOCLAW_AGENTS_DEFAULTS_SYSTEM_FILES_PATH=/etc/picoclaw
```

**Default:** empty string (feature disabled, zero behaviour change).

---

## File Discovery

When `system_files_path` is set, PicoClaw:

1. Recursively walks the directory
2. Collects all files with a `.md` extension (case-insensitive)
3. Sorts them by their **full relative path** (lexicographic)
4. Reads and concatenates their content

Non-`.md` files (`.txt`, `.yaml`, `.json`, etc.) are silently ignored.

### Ordering with numeric prefixes

Files are sorted by relative path, so numeric prefixes give predictable ordering:

```
/etc/picoclaw/
    00-core-behavior.md        ← loaded first
    10-safety-guidelines.md
    20-product-features.md
    30-restrictions.md
    integrations/
        api-usage.md           ← loaded after root files (path: integrations/api-usage.md)
```

### Nested directories

Subdirectories are fully supported. The section header in the prompt uses the relative path:

```markdown
## integrations/api-usage.md

Content of the file...
```

---

## System Prompt Structure

System instructions are injected **after** user bootstrap files and **before** skills:

```
1. Core Identity          (hardcoded)
2. User Bootstrap Files   (AGENTS.md, SOUL.md, USER.md, IDENTITY.md — workspace, editable)
3. System Instructions    ← injected from system_files_path
4. Skills Summary
5. Memory
```

Placing system instructions after user bootstrap files means they act as a **final layer** — platform policies and restrictions apply on top of user-configured identity and behaviour. LLMs give higher weight to later instructions, making system restrictions harder to circumvent via user prompts.

---

## Cache Behaviour

The context cache (`BuildSystemPromptWithCache`) automatically invalidates when any of the following occur in `system_files_path`:

| Event | Detection |
|---|---|
| File modified | mtime change detected on next prompt build |
| New `.md` file created | existence snapshot diff triggers rebuild |
| `.md` file deleted | existence snapshot diff triggers rebuild |

No restart or explicit invalidation is required. Changes propagate on the next request after the filesystem change is observed.

---

## Kubernetes Deployment

The intended K8s pattern:

```yaml
volumes:
  - name: picoclaw-system
    configMap:
      name: picoclaw-system
      defaultMode: 0o444      # r--r--r-- read-only

volumeMounts:
  - name: picoclaw-system
    mountPath: /etc/picoclaw
    readOnly: true
```

With `config.json`:
```json
{
  "agents": {
    "defaults": {
      "system_files_path": "/etc/picoclaw"
    }
  },
  "tools": {
    "allow_read_paths": ["^/etc/picoclaw/"]
  }
}
```

The `allow_read_paths` entry lets the agent read system files via the `read_file` tool (for skills that reference system documents). The OS read-only mount prevents any writes.

### Updating system files without user impact

The backend can update the `picoclaw-system` ConfigMap at any time. K8s propagates the change to the pod's filesystem. On the next agent request, PicoClaw detects the mtime change and rebuilds the system prompt with the new content. The user's workspace (`/root/.picoclaw/workspace/`) is never touched.

---

## Example Use Cases

**Platform behaviour rules:**
```markdown
# 00-core-behavior.md
You are deployed as part of the Acme platform.
Always identify yourself as "Acme Assistant" in your responses.
```

**Safety policies:**
```markdown
# 10-safety.md
Never discuss competitor products.
Do not provide financial or legal advice.
```

**Feature announcements:**
```markdown
# 20-features.md
## New: Image Generation
You now support image generation via the `generate_image` tool.
```

**Hard restrictions:**
```markdown
# 30-restrictions.md
You must not execute system commands that modify files outside the workspace.
```

---

## Local Development

For local/desktop use, leave `system_files_path` unset (empty). All behaviour is identical to PicoClaw without this feature. There is no performance overhead when the field is empty.

To test system files locally:

```json
{
  "agents": {
    "defaults": {
      "system_files_path": "/path/to/my/system-files"
    }
  }
}
```

Or via environment variable:

```bash
PICOCLAW_AGENTS_DEFAULTS_SYSTEM_FILES_PATH=/path/to/my/system-files picoclaw
```
