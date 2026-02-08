package heartbeat

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/picobot/internal/chat"
)

// StartHeartbeat starts a periodic check that reads HEARTBEAT.md and pushes
// its content into the agent's inbound chat hub for processing.
func StartHeartbeat(ctx context.Context, workspace string, interval time.Duration, hub *chat.Hub) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		log.Printf("heartbeat: started (every %v)", interval)
		for {
			select {
			case <-ctx.Done():
				log.Println("heartbeat: stopping")
				return
			case <-ticker.C:
				path := filepath.Join(workspace, "HEARTBEAT.md")
				data, err := os.ReadFile(path)
				if err != nil {
					// file doesn't exist or can't be read — skip silently
					continue
				}
				content := strings.TrimSpace(string(data))
				if content == "" {
					continue
				}

				// Push heartbeat content into the agent loop for processing
				log.Println("heartbeat: sending tasks to agent")
				hub.In <- chat.Inbound{
					Channel:  "heartbeat",
					ChatID:   "system",
					SenderID: "heartbeat",
					Content:  "[HEARTBEAT CHECK] Review and execute any pending tasks from HEARTBEAT.md:\n\n" + content,
				}
			}
		}
	}()
}
