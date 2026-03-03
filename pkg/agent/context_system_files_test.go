package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSystemFiles_LoadedIntoPrompt verifies that .md files from the system
// files directory are loaded into the system prompt under the System
// Instructions section.
func TestSystemFiles_LoadedIntoPrompt(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	os.WriteFile(filepath.Join(sysDir, "00-core.md"), []byte("Core behavior rules."), 0o644)
	os.WriteFile(filepath.Join(sysDir, "10-safety.md"), []byte("Safety guidelines."), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	prompt := cb.BuildSystemPrompt()

	if !strings.Contains(prompt, "# System Instructions") {
		t.Error("prompt should contain System Instructions section")
	}
	if !strings.Contains(prompt, "Core behavior rules.") {
		t.Error("prompt should contain content from 00-core.md")
	}
	if !strings.Contains(prompt, "Safety guidelines.") {
		t.Error("prompt should contain content from 10-safety.md")
	}
}

// TestSystemFiles_SortedByPath verifies that system files are loaded in
// lexicographic order by relative path.
func TestSystemFiles_SortedByPath(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	os.WriteFile(filepath.Join(sysDir, "20-second.md"), []byte("SECOND"), 0o644)
	os.WriteFile(filepath.Join(sysDir, "10-first.md"), []byte("FIRST"), 0o644)
	os.WriteFile(filepath.Join(sysDir, "30-third.md"), []byte("THIRD"), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	content := cb.LoadSystemFiles()

	firstIdx := strings.Index(content, "FIRST")
	secondIdx := strings.Index(content, "SECOND")
	thirdIdx := strings.Index(content, "THIRD")

	if firstIdx < 0 || secondIdx < 0 || thirdIdx < 0 {
		t.Fatalf("not all files found in output: first=%d, second=%d, third=%d", firstIdx, secondIdx, thirdIdx)
	}
	if !(firstIdx < secondIdx && secondIdx < thirdIdx) {
		t.Errorf("files not in sorted order: first=%d, second=%d, third=%d", firstIdx, secondIdx, thirdIdx)
	}
}

// TestSystemFiles_RecursiveDiscovery verifies that .md files in nested
// subdirectories are discovered and included.
func TestSystemFiles_RecursiveDiscovery(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	os.WriteFile(filepath.Join(sysDir, "root.md"), []byte("ROOT"), 0o644)
	os.MkdirAll(filepath.Join(sysDir, "nested", "deep"), 0o755)
	os.WriteFile(filepath.Join(sysDir, "nested", "sub.md"), []byte("NESTED"), 0o644)
	os.WriteFile(filepath.Join(sysDir, "nested", "deep", "leaf.md"), []byte("DEEP"), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	content := cb.LoadSystemFiles()

	if !strings.Contains(content, "ROOT") {
		t.Error("should contain root-level file content")
	}
	if !strings.Contains(content, "NESTED") {
		t.Error("should contain nested file content")
	}
	if !strings.Contains(content, "DEEP") {
		t.Error("should contain deeply nested file content")
	}
	// Verify relative paths are used as section headers
	if !strings.Contains(content, "nested/sub.md") {
		t.Error("should use relative path as section header for nested file")
	}
	if !strings.Contains(content, "nested/deep/leaf.md") {
		t.Error("should use relative path as section header for deeply nested file")
	}
}

// TestSystemFiles_NonMdFilesIgnored verifies that non-.md files in the system
// files directory are skipped.
func TestSystemFiles_NonMdFilesIgnored(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	os.WriteFile(filepath.Join(sysDir, "valid.md"), []byte("VALID"), 0o644)
	os.WriteFile(filepath.Join(sysDir, "skip.txt"), []byte("SKIP_TXT"), 0o644)
	os.WriteFile(filepath.Join(sysDir, "skip.json"), []byte("SKIP_JSON"), 0o644)
	os.WriteFile(filepath.Join(sysDir, "skip.yaml"), []byte("SKIP_YAML"), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	content := cb.LoadSystemFiles()

	if !strings.Contains(content, "VALID") {
		t.Error("should contain .md file content")
	}
	if strings.Contains(content, "SKIP_TXT") {
		t.Error("should not contain .txt file content")
	}
	if strings.Contains(content, "SKIP_JSON") {
		t.Error("should not contain .json file content")
	}
	if strings.Contains(content, "SKIP_YAML") {
		t.Error("should not contain .yaml file content")
	}
}

// TestSystemFiles_EmptyPath_NoEffect verifies that when systemFilesPath is
// empty, no System Instructions section appears in the prompt.
func TestSystemFiles_EmptyPath_NoEffect(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"IDENTITY.md": "# Identity\nTest agent.",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir, "")
	prompt := cb.BuildSystemPrompt()

	if strings.Contains(prompt, "System Instructions") {
		t.Error("prompt should not contain System Instructions when systemFilesPath is empty")
	}
	// Verify bootstrap files still work
	if !strings.Contains(prompt, "Test agent.") {
		t.Error("bootstrap files should still be loaded")
	}
}

// TestSystemFiles_CacheInvalidation_FileModified verifies that modifying a
// system file invalidates the cached system prompt.
func TestSystemFiles_CacheInvalidation_FileModified(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	sysFile := filepath.Join(sysDir, "instructions.md")
	os.WriteFile(sysFile, []byte("Version 1"), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	sp1 := cb.BuildSystemPromptWithCache()

	if !strings.Contains(sp1, "Version 1") {
		t.Fatal("initial prompt should contain Version 1")
	}

	// Modify file with future mtime
	os.WriteFile(sysFile, []byte("Version 2"), 0o644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(sysFile, future, future)

	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked should detect system file modification")
	}

	sp2 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp2, "Version 2") {
		t.Error("rebuilt prompt should contain Version 2")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when system file is modified")
	}
}

// TestSystemFiles_CacheInvalidation_FileCreated verifies that creating a new
// .md file in the system directory invalidates the cache.
func TestSystemFiles_CacheInvalidation_FileCreated(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	os.WriteFile(filepath.Join(sysDir, "existing.md"), []byte("Existing"), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	sp1 := cb.BuildSystemPromptWithCache()

	if strings.Contains(sp1, "New content") {
		t.Fatal("initial prompt should not contain new file content")
	}

	// Create new file
	newFile := filepath.Join(sysDir, "new-file.md")
	os.WriteFile(newFile, []byte("New content"), 0o644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(newFile, future, future)

	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked should detect new system file")
	}

	sp2 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp2, "New content") {
		t.Error("rebuilt prompt should contain new file content")
	}
}

// TestSystemFiles_CacheInvalidation_FileDeleted verifies that deleting a
// system file invalidates the cache.
func TestSystemFiles_CacheInvalidation_FileDeleted(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	deleteMe := filepath.Join(sysDir, "delete-me.md")
	os.WriteFile(deleteMe, []byte("TO BE DELETED"), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	sp1 := cb.BuildSystemPromptWithCache()

	if !strings.Contains(sp1, "TO BE DELETED") {
		t.Fatal("initial prompt should contain file content")
	}

	os.Remove(deleteMe)

	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked should detect deleted system file")
	}

	sp2 := cb.BuildSystemPromptWithCache()
	if strings.Contains(sp2, "TO BE DELETED") {
		t.Error("rebuilt prompt should not contain deleted file content")
	}
}

// TestSystemFiles_CoexistsWithBootstrapFiles verifies that system files appear
// in the prompt alongside workspace bootstrap files, in the correct order.
func TestSystemFiles_CoexistsWithBootstrapFiles(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":   "# User Agent Config",
		"IDENTITY.md": "# User Identity",
	})
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	os.WriteFile(filepath.Join(sysDir, "platform-rules.md"), []byte("Platform rules content."), 0o644)

	cb := NewContextBuilder(tmpDir, sysDir)
	prompt := cb.BuildSystemPrompt()

	// All sections present
	if !strings.Contains(prompt, "User Agent Config") {
		t.Error("prompt should contain workspace bootstrap content")
	}
	if !strings.Contains(prompt, "User Identity") {
		t.Error("prompt should contain workspace identity content")
	}
	if !strings.Contains(prompt, "Platform rules content.") {
		t.Error("prompt should contain system file content")
	}

	// System instructions appear AFTER bootstrap files
	bootstrapIdx := strings.Index(prompt, "User Agent Config")
	systemIdx := strings.Index(prompt, "# System Instructions")
	if systemIdx < bootstrapIdx {
		t.Error("System Instructions should appear after bootstrap files")
	}
}

// TestSystemFiles_EmptyDirectory verifies that when the system files directory
// exists but contains no .md files, no System Instructions section is added.
func TestSystemFiles_EmptyDirectory(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	sysDir := t.TempDir()
	// Directory exists but is empty

	cb := NewContextBuilder(tmpDir, sysDir)
	prompt := cb.BuildSystemPrompt()

	if strings.Contains(prompt, "System Instructions") {
		t.Error("prompt should not contain System Instructions for empty system directory")
	}
}
