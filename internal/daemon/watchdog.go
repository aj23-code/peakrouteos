package daemon

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The service watchdog. systemd restarts a service that stops saying it is alive (WatchdogSec in peakroute.service); this says so every
// third of that time, but only while the daemon really answers: a probe fetches /healthz from its own HTTPS server, so a daemon whose
// web server has wedged (or that is stuck so that nothing is scheduled) stops saying it and is restarted, and a router that stopped
// answering its own web interface comes back by itself. A restart is safe at any moment: a commit that waits for confirmation is kept on
// disk and picked up again (or reverted) by the next start.
//
// The probe deliberately touches nothing but the web server: no configuration lock, no command, so a commit that is being applied for
// a minute (a slow service restart) is never mistaken for a hang.

// startupGrace is how long the watchdog is fed without a probe having to succeed: the first start makes the certificate and reads
// everything, and must not be killed for being slow at it.
const startupGrace = 90 * time.Second

// notifyState sends a message to systemd (sd_notify): a datagram to the socket named in NOTIFY_SOCKET.
func notifyState(socket, state string) error {
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + socket[1:] // an abstract socket
	}
	conn, err := net.Dial("unixgram", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// watchdogInterval is how often to say "alive", from the environment systemd gives a service that has WatchdogSec: a third of the
// time it allows. ok is false when there is no watchdog for this process (not started by systemd, or no WatchdogSec).
func watchdogInterval(getenv func(string) string, pid int) (time.Duration, bool) {
	usec, err := strconv.ParseInt(getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 || getenv("NOTIFY_SOCKET") == "" {
		return 0, false
	}
	if p := getenv("WATCHDOG_PID"); p != "" && p != strconv.Itoa(pid) {
		return 0, false // meant for another process
	}
	every := time.Duration(usec) * time.Microsecond / 3
	if every < time.Second {
		every = time.Second
	}
	return every, true
}

// RunWatchdog feeds systemd's watchdog until ctx ends. probe says whether the daemon is well; notify sends the message (the real one is
// notifyState). Nothing happens when systemd is not watching.
func RunWatchdog(ctx context.Context, every time.Duration, grace time.Duration, probe func(context.Context) error, notify func(string) error, logf func(string, ...any)) {
	started := time.Now()
	failing := false
	feed := func() {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := probe(pctx)
		cancel()
		if err != nil && time.Since(started) > grace {
			if !failing {
				logf("watchdog: the web server does not answer (%v): not feeding the watchdog, systemd will restart the daemon", err)
			}
			failing = true
			return
		}
		failing = false
		if err := notify("WATCHDOG=1"); err != nil {
			logf("watchdog: cannot tell systemd: %v", err)
		}
	}
	feed()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			feed()
		}
	}
}

// healthProbe asks the daemon's own HTTPS server for /healthz over the loopback (its certificate is its own and made for the router's
// names, so it is not checked here: this is a question to itself).
func healthProbe(port string) func(context.Context) error {
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
	addr := "127.0.0.1" + port
	if !strings.HasPrefix(port, ":") {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/healthz", nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("/healthz answered %s", res.Status)
		}
		return nil
	}
}

// startWatchdog begins feeding systemd's watchdog when systemd asks for one.
func (c *Core) startWatchdog() {
	every, ok := watchdogInterval(os.Getenv, os.Getpid())
	if !ok {
		return
	}
	socket := os.Getenv("NOTIFY_SOCKET")
	go RunWatchdog(c.Ctx, every, startupGrace, healthProbe(c.Port), func(state string) error { return notifyState(socket, state) }, log.Printf)
}
