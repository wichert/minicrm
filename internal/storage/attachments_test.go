package storage_test

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wichert/minicrm/internal/storage"
)

func TestStoreSaveOpenAndDeduplicate(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	baseDir := t.TempDir()
	store, err := storage.NewStore(baseDir, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ctx := context.Background()
	payload := "hello world attachment content"
	blob1, err := store.Save(ctx, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if blob1.SizeBytes != int64(len(payload)) {
		t.Fatalf("got size %d, want %d", blob1.SizeBytes, len(payload))
	}

	// Saving identical payload deduplicates to the exact same path and digest.
	blob2, err := store.Save(ctx, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if blob1.SHA256Hex != blob2.SHA256Hex || blob1.StorageRelPath != blob2.StorageRelPath {
		t.Fatalf("expected deduplicated blob: %+v vs %+v", blob1, blob2)
	}

	f, err := store.Open(ctx, blob1.StorageRelPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	readBack, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(readBack) != payload {
		t.Fatalf("got %q, want %q", string(readBack), payload)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"event_category":"data_change"`) || !strings.Contains(logs, `"event_category":"data_access"`) {
		t.Fatalf("expected structured data_change and data_access logs, got: %s", logs)
	}
	if !strings.Contains(logs, `"operation":"reuse"`) {
		t.Fatalf("expected a blob reuse data_change log for identical content, got: %s", logs)
	}

	// Only a single copy of the identical payload may exist on disk.
	fileCount := 0
	err = filepath.WalkDir(filepath.Join(baseDir, blob1.StorageRelPath[:2]), func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			fileCount++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if fileCount != 1 {
		t.Fatalf("expected exactly 1 stored blob file, found %d", fileCount)
	}
}

func TestStoreRejectsPathTraversal(t *testing.T) {
	store, err := storage.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "ab/../../../etc/passwd", "."} {
		if _, err := store.Open(ctx, bad); err == nil {
			t.Fatalf("expected traversal error for %q, got nil", bad)
		}
	}
}
