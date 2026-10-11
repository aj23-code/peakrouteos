package syslogfwd

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// FromJournalLine reads one line of `journalctl --output=json` into a message. The router's own host name is used when the
// journal entry carries none. ok is false for a line that is not an entry or has no text.
func FromJournalLine(line []byte, host string) (Message, bool) {
	var raw struct {
		Message    json.RawMessage `json:"MESSAGE"`
		Priority   string          `json:"PRIORITY"`
		Facility   string          `json:"SYSLOG_FACILITY"`
		Identifier string          `json:"SYSLOG_IDENTIFIER"`
		Comm       string          `json:"_COMM"`
		PID        string          `json:"_PID"`
		Timestamp  string          `json:"__REALTIME_TIMESTAMP"`
		Transport  string          `json:"_TRANSPORT"`
	}
	if json.Unmarshal(line, &raw) != nil || len(raw.Message) == 0 {
		return Message{}, false
	}
	text := ""
	if raw.Message[0] == '"' {
		if json.Unmarshal(raw.Message, &text) != nil {
			return Message{}, false
		}
	} else { // the journal writes text that is not valid UTF-8 as a list of bytes
		var b []byte
		if json.Unmarshal(raw.Message, &b) != nil {
			return Message{}, false
		}
		text = strings.ToValidUTF8(string(b), "?")
	}
	if strings.TrimSpace(text) == "" {
		return Message{}, false
	}
	m := Message{Time: time.Now(), Facility: 1, Severity: 6, Host: host, ProcID: raw.PID, Text: text}
	if us, err := strconv.ParseInt(raw.Timestamp, 10, 64); err == nil {
		m.Time = time.UnixMicro(us)
	}
	if n, err := strconv.Atoi(raw.Priority); err == nil && n >= 0 && n <= 7 {
		m.Severity = n
	}
	if n, err := strconv.Atoi(raw.Facility); err == nil && n >= 0 && n <= 23 {
		m.Facility = n
	} else if raw.Transport == "kernel" {
		m.Facility = 0
	}
	m.App = raw.Identifier
	if m.App == "" {
		m.App = raw.Comm
	}
	return m, true
}

// IsFirewallLine reports whether a journal message is a firewall rule's log line (the firewall has its own stream).
func IsFirewallLine(text string) bool { return strings.HasPrefix(text, "fw:") }
