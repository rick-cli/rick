package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"rick/internal/provider"
)

// ArchiveTrimmed appends the messages that were folded out of the
// provider-facing view (head trim, distill fold, prune) to a JSONL archive
// under archiveDir. The canonical transcript is never modified; this is a
// durable side channel so dropped originals stay traceable, mirroring
// reasonix's fold/prune archival. One timestamped file per operation keeps
// replay simple and avoids contending writers.
func ArchiveTrimmed(archiveDir, sessionID, reason string, messages []provider.Message) error {
	if archiveDir == "" || len(messages) == 0 {
		return nil
	}
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("fold-%s-%s.jsonl", time.Now().Format("20060102-150405.000"), sessionID)
	path := filepath.Join(archiveDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	record := struct {
		Time    string           `json:"time"`
		Reason  string           `json:"reason"`
		Session string           `json:"session_id"`
		Message provider.Message `json:"message"`
	}{Time: time.Now().Format(time.RFC3339Nano), Reason: reason, Session: sessionID}
	for _, message := range messages {
		record.Message = message
		if err := enc.Encode(record); err != nil {
			return err
		}
	}
	return nil
}
