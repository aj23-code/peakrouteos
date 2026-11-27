package failover

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// The last changes are kept in a small file, so that the page still shows what happened before the daemon was restarted (an update, a
// reboot). The caller holds the lock.

// loadEvents reads the file once.
func (c *Controller) loadEvents() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.EventsPath == "" {
		return
	}
	data, err := os.ReadFile(c.EventsPath)
	if err != nil {
		return
	}
	var ev []Event
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	if len(ev) > maxEvents {
		ev = ev[:maxEvents]
	}
	// what is on disk is older than anything that happened since the start
	c.events = append(c.events, ev...)
	if len(c.events) > maxEvents {
		c.events = c.events[:maxEvents]
	}
}

// saveEvents writes the file (a failure is not an error: the log has the lines too).
func (c *Controller) saveEvents() {
	if c.EventsPath == "" {
		return
	}
	data, err := json.Marshal(c.events)
	if err != nil {
		return
	}
	tmp := c.EventsPath + ".tmp"
	if os.MkdirAll(filepath.Dir(c.EventsPath), 0755) != nil || os.WriteFile(tmp, data, 0600) != nil {
		return
	}
	os.Rename(tmp, c.EventsPath)
}
