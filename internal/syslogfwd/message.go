// Package syslogfwd sends the router's logs to syslog servers: the system journal (everything the system says) and the
// firewall log (the packets that rules with logging matched, with the rule's name and action). docs/settings.md describes it.
package syslogfwd

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Message is one log line on its way out.
type Message struct {
	Time     time.Time
	Facility int // 0 to 23
	Severity int // 0 to 7
	Host     string
	App      string
	ProcID   string
	MsgID    string
	Text     string
}

const (
	maxText = 1900 // bytes of message text: well inside what UDP and every receiver takes
)

// printable makes a header field safe for RFC 5424: printable ASCII without spaces, at most max characters, "-" when empty.
func printable(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r > 32 && r < 127 {
			b.WriteRune(r)
		} else if r != '\n' {
			b.WriteByte('_')
		}
		if b.Len() >= max {
			break
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

// cleanText keeps the text on one line and within the size limit (cut at a character boundary).
func cleanText(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\x00", " ").Replace(s)
	if len(s) > maxText {
		s = s[:maxText]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
	}
	return s
}

// Format writes the message the way RFC 5424 says: <PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA MSG.
func (m Message) Format() string {
	fac, sev := m.Facility, m.Severity
	if fac < 0 || fac > 23 {
		fac = 1
	}
	if sev < 0 || sev > 7 {
		sev = 6
	}
	ts := m.Time.UTC().Format("2006-01-02T15:04:05.000000Z")
	return fmt.Sprintf("<%d>1 %s %s %s %s %s - %s", fac*8+sev, ts, printable(m.Host, 255), printable(m.App, 48), printable(m.ProcID, 128), printable(m.MsgID, 32), cleanText(m.Text))
}

// quoteValue writes a value for a key=value line: bare when it is a plain word, quoted (with \" and \\) otherwise.
func quoteValue(v string) string {
	plain := v != ""
	for _, r := range v {
		if r <= 32 || r >= 127 || r == '"' || r == '\\' || r == '=' {
			plain = false
		}
	}
	if plain {
		return v
	}
	return strconv.Quote(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, v))
}
