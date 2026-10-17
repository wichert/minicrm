// Package storage provides content-addressed local filesystem storage for email attachments.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// Store manages content-addressed attachment files on local disk.
type Store struct {
	baseDir string
	logger  *slog.Logger
}

// StoredBlob holds the metadata computed while writing a blob to disk.
type StoredBlob struct {
	SHA256Hex      string
	StorageRelPath string
	SizeBytes      int64
}

// NewStore initializes the attachment storage directory layout under baseDir.
func NewStore(baseDir string, logger *slog.Logger) (*Store, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, errors.New("attachments baseDir must not be empty")
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve attachments baseDir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absBase, "tmp"), 0o750); err != nil {
		return nil, fmt.Errorf("create attachments tmp dir: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		baseDir: absBase,
		logger:  logger,
	}, nil
}

// Save streams r to a temporary file, computes its SHA-256 digest, and atomically moves it
// into the content-addressed path <sha256[0:2]>/<sha256[2:4]>/<sha256>.
func (s *Store) Save(ctx context.Context, r io.Reader) (StoredBlob, error) {
	tmpPath := filepath.Join(s.baseDir, "tmp", uuid.NewString()+".part")
	// #nosec G304 -- tmpPath is constructed inside s.baseDir/tmp using a generated UUID
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return StoredBlob{}, fmt.Errorf("create temp attachment file: %w", err)
	}
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hasher), r)
	if copyErr != nil {
		_ = f.Close()
		return StoredBlob{}, fmt.Errorf("write temp attachment: %w", copyErr)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return StoredBlob{}, fmt.Errorf("fsync temp attachment: %w", err)
	}
	if err := f.Close(); err != nil {
		return StoredBlob{}, fmt.Errorf("close temp attachment: %w", err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	relPath := filepath.Join(digest[0:2], digest[2:4], digest)
	finalPath, err := s.resolveSafePath(relPath)
	if err != nil {
		return StoredBlob{}, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return StoredBlob{}, fmt.Errorf("create attachment fanout dir: %w", err)
	}

	// Content-addressed storage is idempotent: when the same bytes (for example an
	// attachment delivered to several mail accounts) were already stored, reuse the
	// existing blob instead of writing a second copy.
	operation := "create"
	if _, statErr := os.Stat(finalPath); statErr == nil {
		operation = "reuse"
		_ = os.Remove(tmpPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return StoredBlob{}, fmt.Errorf("stat attachment blob: %w", statErr)
	} else if err := os.Rename(tmpPath, finalPath); err != nil {
		// A concurrent writer may have created the same blob first.
		if _, statErr := os.Stat(finalPath); statErr == nil {
			operation = "reuse"
			_ = os.Remove(tmpPath)
		} else {
			return StoredBlob{}, fmt.Errorf("rename temp attachment to final path: %w", err)
		}
	}

	s.logger.InfoContext(ctx, "data_change",
		slog.String("event_category", "data_change"),
		slog.String("operation", operation),
		slog.String("resource_type", "attachment_blob"),
		slog.String("sha256", digest),
		slog.Int64("size_bytes", n),
		slog.String("actor_user_id", sqlc.ActorFromContext(ctx)),
	)

	return StoredBlob{
		SHA256Hex:      digest,
		StorageRelPath: relPath,
		SizeBytes:      n,
	}, nil
}

// Open opens a stored attachment blob for reading, verifying path confinement under baseDir.
func (s *Store) Open(ctx context.Context, relPath string) (*os.File, error) {
	fullPath, err := s.resolveSafePath(relPath)
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- fullPath is verified by resolveSafePath to be confined within s.baseDir
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("open attachment blob: %w", err)
	}

	s.logger.InfoContext(ctx, "data_access",
		slog.String("event_category", "data_access"),
		slog.String("operation", "read"),
		slog.String("resource_type", "attachment_blob"),
		slog.String("storage_relpath", relPath),
		slog.String("actor_user_id", sqlc.ActorFromContext(ctx)),
	)

	return f, nil
}

func (s *Store) resolveSafePath(relPath string) (string, error) {
	cleaned := filepath.Clean(relPath)
	if cleaned == "." || cleaned == ".." || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("invalid attachment relative path %q", relPath)
	}
	full := filepath.Join(s.baseDir, cleaned)
	rel, err := filepath.Rel(s.baseDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("attachment path escapes baseDir: %q", relPath)
	}
	return full, nil
}
