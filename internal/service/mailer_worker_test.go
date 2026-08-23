// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// flakyStub fails first n connections then behaves as normal stub.
type flakyStub struct {
	ln        net.Listener
	msgs      chan string
	done      chan struct{}
	failCount int
	accepted  atomic.Int64
}

func newFlakyStub(t *testing.T, failTimes int) *flakyStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &flakyStub{ln: ln, msgs: make(chan string, 10), done: make(chan struct{}), failCount: failTimes}
	go s.serve()
	t.Cleanup(func() { ln.Close(); <-s.done })
	return s
}

func (s *flakyStub) serve() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		n := s.accepted.Add(1)
		if int(n) <= s.failCount {
			// fail: close immediately without SMTP handshake -> client gets error
			c.Close()
			continue
		}
		go handleSMTPConn(c, s.msgs)
	}
}

func flakyPort(s *flakyStub) string { return strconv.Itoa(s.ln.Addr().(*net.TCPAddr).Port) }

func TestMailWorkerDeliversRenderedCode(t *testing.T) {
	fs := newFakeStore()
	svc, mw := newMailServiceWithWorker(t, fs)
	stub := newSMTPStub(t, false)
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	_, err := fs.UpsertEmailTemplate(context.Background(), string(domain.EmailTemplateRegisterCode), "subj {{code}}", "body {{code}} ttl={{ttl_minutes}} app={{app_name}}")
	require.NoError(t, err)
	require.NoError(t, mw.Enqueue(MailSendTask{To: "to@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "445566", TTLMin: 10}))
	select {
	case msg := <-stub.msgs:
		require.Contains(t, msg, "445566")
		require.Contains(t, msg, "from@example.com")
	case <-time.After(3 * time.Second):
		require.Fail(t, "did not receive mail")
	}
	// wait for sent counter to be visible (msg channel filled before sent++).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if mw.Stats().(mailStats).SentTotal == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int64(1), mw.Stats().(mailStats).SentTotal)
}

func TestMailWorkerRetrySuccessAfterTwoFails(t *testing.T) {
	origBackoff := mailRetryBackoff
	mailRetryBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { mailRetryBackoff = origBackoff })

	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	t.Cleanup(func() { _ = mw.Close(context.Background()) })
	svc.SetMailEnqueue(mw.Enqueue)

	stub := newFlakyStub(t, 2)
	port := flakyPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	_, err := fs.UpsertEmailTemplate(context.Background(), string(domain.EmailTemplateRegisterCode), "s {{code}}", "b {{code}}")
	require.NoError(t, err)

	require.NoError(t, mw.Enqueue(MailSendTask{To: "to@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "778899", TTLMin: 10}))

	// wait for delivery (3 attempts with short backoff)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(stub.msgs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case msg := <-stub.msgs:
		require.Contains(t, msg, "778899")
	default:
		require.Fail(t, "expected delivery after retries")
	}
	st := mw.Stats().(mailStats)
	require.Equal(t, int64(1), st.SentTotal)
	require.Equal(t, int64(2), st.RetryTotal, "two retries before success")
	require.Equal(t, int64(0), st.FailedTotal)
}

func TestMailWorkerAllFailFailedAndLastErr(t *testing.T) {
	origTimeout := mailSendTimeout
	mailSendTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mailSendTimeout = origTimeout })
	origBackoff := mailRetryBackoff
	mailRetryBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { mailRetryBackoff = origBackoff })

	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	t.Cleanup(func() { _ = mw.Close(context.Background()) })
	svc.SetMailEnqueue(mw.Enqueue)

	stub := newSMTPStub(t, true) // hang -> timeout each attempt
	_ = stub
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})

	require.NoError(t, mw.Enqueue(MailSendTask{To: "to@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "000111", TTLMin: 10}))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if mw.Stats().(mailStats).FailedTotal > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := mw.Stats().(mailStats)
	require.Equal(t, int64(1), st.FailedTotal)
	require.NotEmpty(t, st.LastError)
	require.Equal(t, int64(2), st.RetryTotal)
}

func TestMailWorkerQueueFullDropped(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	// Do NOT start -> ch not drained, fill to capacity
	for i := 0; i < mailQueueCap; i++ {
		require.NoError(t, mw.Enqueue(MailSendTask{To: "a@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "123456", TTLMin: 10}))
	}
	err := mw.Enqueue(MailSendTask{To: "overflow@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "999999", TTLMin: 10})
	require.ErrorIs(t, err, ErrMailQueueFull)
	require.Equal(t, int64(1), mw.Stats().(mailStats).DroppedTotal)
}

func TestMailWorkerCloseThenEnqueueDropped(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	require.NoError(t, mw.Close(context.Background()))
	err := mw.Enqueue(MailSendTask{To: "after@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "111111", TTLMin: 10})
	require.ErrorIs(t, err, ErrMailQueueFull)
	require.GreaterOrEqual(t, mw.Stats().(mailStats).DroppedTotal, int64(1))
	// double Close idempotent
	require.NoError(t, mw.Close(context.Background()))
}

func TestMailWorkerShutdownDrain(t *testing.T) {
	origBackoff := mailRetryBackoff
	mailRetryBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { mailRetryBackoff = origBackoff })

	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	stub := newSMTPStub(t, false)
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	// enqueue 3 tasks
	for i := 0; i < 3; i++ {
		require.NoError(t, mw.Enqueue(MailSendTask{To: "u" + strconv.Itoa(i) + "@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "code" + strconv.Itoa(i), TTLMin: 10}))
	}
	// brief wait for at least one to be processed, then Close with budget
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, mw.Close(ctx))
	// after close, at most 3 sent or dropped, no leak (worker loop exited)
	st := mw.Stats().(mailStats)
	require.Equal(t, int64(0), int64(st.Queued))
	// total of sent + dropped should account for 3
	total := st.SentTotal + st.DroppedTotal + st.FailedTotal
	require.Equal(t, int64(3), total, "每个入队任务必须恰好一个终态")
	// verify msgs received (at least 1, up to 3 within budget)
	received := len(stub.msgs)
	// drain remaining msgs without blocking
	for {
		select {
		case <-stub.msgs:
			received++
		default:
			goto done
		}
	}
done:
	// If all drained within budget, received should be 3 or fewer if budget short; but with 2s budget and real SMTP, should be 3
	require.GreaterOrEqual(t, received, 1)
	// ensure double close safe
	require.NoError(t, mw.Close(context.Background()))
}

func TestMailWorkerEnqueueAfterQuitDroppedCount(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	// close then try many enqueues, all dropped
	require.NoError(t, mw.Close(context.Background()))
	for i := 0; i < 5; i++ {
		err := mw.Enqueue(MailSendTask{To: "x@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: strings.Repeat("1", 6), TTLMin: 10})
		require.ErrorIs(t, err, ErrMailQueueFull)
	}
	require.Equal(t, int64(5), mw.Stats().(mailStats).DroppedTotal)
}
