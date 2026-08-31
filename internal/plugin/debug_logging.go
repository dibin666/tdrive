package plugin

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

const pluginDebugLogPath = "/media/dibin/HDD1/Works/tdrive-plugin/.cursor/debug-f51fed.log"

var pluginDebugLogMu sync.Mutex

// writePluginDebugLog appends diagnostics for the cross-process plugin path
// investigation. Callers must not provide credentials, tokens, or command
// output.
func writePluginDebugLog(hypothesisID, location, message string, data map[string]any) {
	payload := map[string]any{
		"sessionId":    "f51fed",
		"hypothesisId": hypothesisID,
		"location":     location,
		"message":      message,
		"data":         data,
		"timestamp":    time.Now().UnixMilli(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}

	pluginDebugLogMu.Lock()
	defer pluginDebugLogMu.Unlock()
	file, err := os.OpenFile(pluginDebugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(encoded, '\n'))
	_ = file.Close()
}
