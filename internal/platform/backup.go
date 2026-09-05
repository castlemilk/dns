package platform

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.etcd.io/bbolt"
)

// MaxBackupBytes bounds the streamed platform store. The route refuses above
// it rather than tying up a connection for minutes: a store this large means
// the janitor is not keeping up, which is the thing to fix.
const MaxBackupBytes = 256 << 20

// backupWriteTimeout is how long the handler gives the client to read the copy.
const backupWriteTimeout = 10 * time.Minute

// BackupTo streams the store to the writer prepare returns. prepare is called
// inside the read transaction with the exact number of bytes that will follow,
// so the handler can set Content-Length truthfully and refuse an oversize store
// before writing anything.
func (s *Store) BackupTo(ctx context.Context, prepare func(size int64) (io.Writer, error)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var written int64
	err := s.viewTx(ctx, "platform_backup", func(tx *bbolt.Tx) error {
		writer, err := prepare(tx.Size())
		if err != nil {
			return err
		}
		if writer == nil {
			return nil
		}
		count, err := tx.WriteTo(writer)
		written = count
		return err
	})
	return written, err
}

type backupHandler struct {
	store  *Store
	logger *slog.Logger
}

// BackupHandler serves GET /internal/v1/platform-backup. The running process
// holds bbolt's exclusive lock and the image has no shell, so the owner of the
// lock is the only thing that can hand out a consistent copy.
func (s *StatusService) BackupHandler() http.Handler {
	return &backupHandler{store: s.deps.Store, logger: s.deps.Log()}
}

// errTooLarge stops the transaction before any byte is written.
type errTooLarge struct{}

func (errTooLarge) Error() string { return "platform store exceeds the backup size limit" }

func (h *backupHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.store == nil {
		http.Error(writer, "the platform store is not open", http.StatusServiceUnavailable)
		return
	}
	controller := http.NewResponseController(writer)
	if err := controller.SetWriteDeadline(time.Now().Add(backupWriteTimeout)); err != nil {
		h.logger.Debug("extend platform backup write deadline", "error", err)
	}

	headOnly := request.Method == http.MethodHead
	_, err := h.store.BackupTo(request.Context(), func(size int64) (io.Writer, error) {
		if size > MaxBackupBytes {
			return nil, errTooLarge{}
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Disposition", `attachment; filename="platform.db"`)
		writer.WriteHeader(http.StatusOK)
		if headOnly {
			return nil, nil
		}
		return writer, nil
	})
	if err == nil {
		return
	}
	if _, ok := err.(errTooLarge); ok {
		http.Error(writer, "platform store exceeds the backup size limit", http.StatusServiceUnavailable)
		return
	}
	// Headers are already written by the time a copy can fail, so the only
	// honest thing left is to log it and let the client's length check fail.
	h.logger.Error("stream platform backup", "error", err)
}
