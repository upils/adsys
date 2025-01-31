package adsys_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubuntu/adsys/cmd/adsysd/client"
	"github.com/ubuntu/adsys/cmd/adsysd/daemon"
)

func TestServiceStop(t *testing.T) {
	tests := map[string]struct {
		polkitAnswer     string
		daemonNotStarted bool
		force            bool

		wantErr bool
	}{
		"Stop daemon":           {polkitAnswer: "yes"},
		"Stop daemon denied":    {polkitAnswer: "no", wantErr: true},
		"Daemon not responding": {daemonNotStarted: true, wantErr: true},

		"Force stop doesn’t exit on error": {polkitAnswer: "yes", force: true, wantErr: false},
	}
	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			polkitAnswer(t, tc.polkitAnswer)

			conf := createConf(t, "")
			if !tc.daemonNotStarted {
				defer runDaemon(t, conf)()
			}

			args := []string{"service", "stop"}
			if tc.force {
				args = append(args, "-f")
			}
			out, err := runClient(t, conf, args...)
			assert.Empty(t, out, "Nothing printed on stdout")
			if tc.wantErr {
				require.Error(t, err, "client should exit with an error")
				return
			}
			require.NoError(t, err, "client should exit with no error")
		})
	}
}

func TestServiceStopWaitForHangingClient(t *testing.T) {
	polkitAnswer(t, "yes")

	conf := createConf(t, "")
	d := daemon.New()
	changeOsArgs(t, conf)

	daemonStopped := make(chan struct{})
	go func() {
		defer close(daemonStopped)
		err := d.Run()
		require.NoError(t, err, "daemon should exit with no error")
	}()
	d.WaitReady()

	outCat, stopCat, err := startCmd(t, false, "adsysctl", "-vv", "-c", conf, "service", "cat")
	require.NoError(t, err, "cat should start successfully")

	// Let cat connecting to daemon and daemon forwarding logs
	time.Sleep(time.Second)

	// Stop without forcing: shouldn’t be able to stop it
	// Don’t use the helper as we don’t need stdout (and cat will trigger the stdout capturer in daemon logs)
	c := client.New()
	restoreArgs := changeOsArgs(t, conf, "service", "stop")
	err = c.Run()
	restoreArgs()
	require.NoError(t, err, "client should exit with no error (graceful stop requested)")

	// Let’s wait 5 seconds to ensure it hadn’t stopped
	select {
	case <-daemonStopped:
		log.Fatal("Daemon stopped when we expected to hang out")
	case <-time.After(5 * time.Second):
	}

	stopCat()
	assert.NotEmpty(t, outCat(), "Cat has captured some outputs")

	// Let’s give it 3 seconds to stop
	select {
	case <-time.After(3 * time.Second):
		log.Fatal("Daemon hadn’t stopped quickly once Cat has quit")
	case <-daemonStopped:
	}
}

func TestServiceStopForcedWithHangingClient(t *testing.T) {
	polkitAnswer(t, "yes")

	conf := createConf(t, "")
	d := daemon.New()
	changeOsArgs(t, conf)

	daemonStopped := make(chan struct{})
	go func() {
		defer close(daemonStopped)
		err := d.Run()
		require.NoError(t, err, "daemon should exit with no error")
	}()
	d.WaitReady()

	outCat, stopCat, err := startCmd(t, false, "adsysctl", "-vv", "-c", conf, "service", "cat")
	require.NoError(t, err, "cat should start successfully")

	// Let cat connecting to daemon and daemon forwarding logs
	time.Sleep(time.Second)

	// Force stop it
	// Don’t use the helper as we don’t need stdout (and cat will trigger the stdout capturer in daemon logs)
	c := client.New()
	restoreArgs := changeOsArgs(t, conf, "service", "stop", "-f")
	err = c.Run()
	restoreArgs()
	require.NoError(t, err, "client should exit with no error")

	select {
	case <-time.After(3 * time.Second):
		t.Fatal("daemon should stop quickly after forced stop")
	case <-daemonStopped:
	}
	stopCat()
	assert.NotEmpty(t, outCat(), "Cat has captured some outputs")
}

func TestServiceCat(t *testing.T) {
	// Unfortunately, we can’t easily create the cat client and other pingers in the same process:
	// as cat will print what was forwarded to it, and the daemon, other clients and such will all write
	// also, this creates multiple calls, with overriding fds and such.

	// Keep coverage by either switching the daemon or the cat client in their own process.
	// Always keep version in its own process.

	tests := map[string]struct {
		polkitAnswer     string
		daemonNotStarted bool
		coverCatClient   bool
		multipleCats     bool

		wantErr bool
	}{
		"Cat other clients and daemon - cover daemon": {polkitAnswer: "yes"},
		"Cat denied - cover daemon":                   {polkitAnswer: "no", wantErr: true},
		"Daemon not responding - cover daemon":        {daemonNotStarted: true, wantErr: true},

		"Cat other clients and daemon - cover client": {polkitAnswer: "yes", coverCatClient: true},
		"Cat denied - cover client":                   {polkitAnswer: "no", coverCatClient: true, wantErr: true},
		"Daemon not responding - cover client":        {daemonNotStarted: true, coverCatClient: true, wantErr: true},

		"Multiple cats": {multipleCats: true, polkitAnswer: "yes"},
	}
	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			polkitAnswer(t, tc.polkitAnswer)

			conf := createConf(t, "")
			if !tc.daemonNotStarted && !tc.coverCatClient {
				defer runDaemon(t, conf)()
			}

			if tc.coverCatClient {
				_, stopDaemon, err := startCmd(t, false, "adsysd", "-c", conf)
				require.NoError(t, err, "daemon should start successfully")
				defer stopDaemon()
				// Let the daemon starting
				time.Sleep(5 * time.Second)
			}

			var outCat, outCat2 func() string
			var stopCat, stopCat2 func() error
			if tc.coverCatClient {
				// create cat client and control it, capturing stderr for logs

				// capture log output (set to stderr, but captured when loading logrus)
				r, w, err := os.Pipe()
				require.NoError(t, err, "Setup: pipe shouldn’t fail")
				orig := logrus.StandardLogger().Out
				logrus.StandardLogger().SetOutput(w)

				c := client.New()

				var out bytes.Buffer
				done := make(chan struct{})
				go func() {
					defer close(done)
					changeOsArgs(t, conf, "service", "cat")
					err = c.Run()
				}()

				outCat = func() string {
					return out.String()
				}
				stopCat = func() error {
					c.Quit()
					<-done
					logrus.StandardLogger().SetOutput(orig)
					w.Close()
					_, errCopy := io.Copy(&out, r)
					require.NoError(t, errCopy, "Couldn’t copy stderr to buffer")
					return errors.New("Mimic cat killing")
				}
			} else {

				var err error
				if tc.multipleCats {
					outCat2, stopCat2, err = startCmd(t, false, "adsysctl", "-vv", "-c", conf, "service", "cat")
					require.NoError(t, err, "cat should start successfully")
				}

				outCat, stopCat, err = startCmd(t, false, "adsysctl", "-vv", "-c", conf, "service", "cat")
				require.NoError(t, err, "cat should start successfully")
			}

			// Let cat connecting to daemon and daemon forwarding logs
			time.Sleep(time.Second)

			// Second client we will spy logs on
			_, _, err := startCmd(t, true, "adsysctl", "-vv", "-c", conf, "version")
			if !tc.wantErr {
				require.NoError(t, err, "version should run successfully")
			}

			err = stopCat()
			require.Error(t, err, "cat has been killed")

			if tc.wantErr {
				assert.NotContains(t, outCat(), "New connection from client", "no internal logs from server are forwarded")
				assert.NotContains(t, outCat(), "New request /service/Version", "no debug logs for clients are forwarded")
				return
			}

			assert.Contains(t, outCat(), "New connection from client", "internal logs from server are forwarded")
			assert.Contains(t, outCat(), "New request /service/Version", "debug logs for clients are forwarded")

			if tc.multipleCats {
				// Give time for the server to forward first Cat closing
				time.Sleep(time.Second)
				err = stopCat2()
				require.Error(t, err, "cat2 has been killed")

				assert.Contains(t, outCat2(), "New connection from client", "internal logs from server are forwarded")
				assert.Contains(t, outCat2(), "New request /service/Cat", "debug logs for the other cat is forwarded")
				assert.Contains(t, outCat2(), "Request /service/Cat done", "the other cat is closed")
				assert.Contains(t, outCat2(), "New request /service/Version", "debug logs for clients are forwarded")
			}
		})
	}
}
