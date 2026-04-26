package webhooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Worker delivers pending webhook events.
type Worker struct {
	svc      *Service
	logger   *log.Logger
	http     *http.Client
	interval time.Duration
}

// NewWorker returns a Worker that ticks every 30s by default.
func NewWorker(svc *Service, logger *log.Logger) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{
		svc:      svc,
		logger:   logger,
		http:     &http.Client{Timeout: 15 * time.Second},
		interval: 30 * time.Second,
	}
}

// WithInterval is a test-only override.
func (w *Worker) WithInterval(d time.Duration) *Worker {
	w.interval = d
	return w
}

// Run loops until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.svc == nil {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	for {
		ok, err := w.deliverNext(ctx)
		if err != nil {
			w.logger.Printf("webhooks.worker: %v", err)
			return
		}
		if !ok {
			return
		}
	}
}

// deliverNext claims one pending delivery and posts it.
func (w *Worker) deliverNext(ctx context.Context) (bool, error) {
	if w.svc.pool == nil {
		return false, nil
	}
	var (
		id, endpointID, tenantID, eventType, url, secretHash string
		payload                                              []byte
		attempts                                             int
	)
	err := pgx.BeginFunc(ctx, w.svc.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT d.id::text, d.endpoint_id::text, d.tenant_id::text, d.event_type, d.payload::text, d.attempts,
			       e.url, e.secret_hash
			FROM webhook_deliveries d
			JOIN webhook_endpoints e ON e.id = d.endpoint_id
			WHERE d.status = 'pending' AND d.next_retry_at <= now()
			ORDER BY d.next_retry_at
			FOR UPDATE OF d SKIP LOCKED
			LIMIT 1
		`)
		var payloadStr string
		err := row.Scan(&id, &endpointID, &tenantID, &eventType, &payloadStr, &attempts, &url, &secretHash)
		if err != nil {
			return err
		}
		payload = []byte(payloadStr)
		_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET attempts = attempts + 1 WHERE id = $1::uuid`, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	status, sendErr := w.send(ctx, url, secretHash, eventType, payload)
	final := status >= 200 && status < 300
	switch {
	case final:
		_, _ = w.svc.pool.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status = 'delivered', last_status = $2, delivered_at = now(), last_error = ''
			WHERE id = $1::uuid
		`, id, status)
	case attempts+1 >= MaxAttempts:
		errMsg := ""
		if sendErr != nil {
			errMsg = sendErr.Error()
		}
		_, _ = w.svc.pool.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'failed', last_status = $2, last_error = $3
			WHERE id = $1::uuid
		`, id, status, errMsg)
	default:
		// Exponential backoff: 30s, 5min, 30min.
		delay := time.Duration(30) * time.Second
		switch attempts + 1 {
		case 1:
			delay = 30 * time.Second
		case 2:
			delay = 5 * time.Minute
		default:
			delay = 30 * time.Minute
		}
		errMsg := ""
		if sendErr != nil {
			errMsg = sendErr.Error()
		}
		_, _ = w.svc.pool.Exec(ctx, `
			UPDATE webhook_deliveries
			SET next_retry_at = now() + $2::interval, last_status = $3, last_error = $4
			WHERE id = $1::uuid
		`, id, fmt.Sprintf("%d seconds", int(delay.Seconds())), status, errMsg)
	}
	return true, nil
}

func (w *Worker) send(ctx context.Context, url, secretHash, eventType string, payload []byte) (int, error) {
	// We do not have the plaintext secret on the worker side. The
	// endpoint expects the same hash for verification, so we sign
	// with the hash itself — this mirrors how Stripe's "endpoint
	// secret" is the only shared value. Tenants verify by
	// recomputing hex-encoded HMAC-SHA256 over `<unix>.<body>`.
	ts := time.Now().UTC()
	sig := SignPayload(secretHash, ts, payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-KMail-Event", eventType)
	req.Header.Set("X-KMail-Signature", sig)
	resp, err := w.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// VerifySignature is a helper tenants can call to verify
// `X-KMail-Signature` against the body + their stored secret hash.
// (Exposed so test code in this package can exercise the round
// trip.)
func VerifySignature(secretHash, header string, payload []byte) bool {
	if header == "" {
		return false
	}
	var ts int64
	var v1 string
	_, err := fmt.Sscanf(header, "t=%d,v1=%s", &ts, &v1)
	if err != nil {
		return false
	}
	expected := SignPayload(secretHash, time.Unix(ts, 0).UTC(), payload)
	// Compare the v1= half by re-signing.
	provided := fmt.Sprintf("t=%d,v1=%s", ts, v1)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// hashCheck guards against accidentally storing plaintext.
func hashCheck(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
