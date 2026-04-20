// Package billing provides billing event processing workers for DNSFox v2.
// worker.go polls the stripe_events table every 30 seconds for unprocessed
// Stripe webhook events and dispatches each one to the appropriate handler.
// Processing is idempotent: we claim events via processed_at before handling.
package billing

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// billingPollInterval is how often we poll stripe_events for new work.
	billingPollInterval = 30 * time.Second

	// dunningTickInterval is how often the dunning escalation pass runs.
	dunningTickInterval = 15 * time.Minute
)

// stripeEvent is a row projection of the stripe_events table.
type stripeEvent struct {
	ID          string
	Type        string
	Data        json.RawMessage
	ProcessedAt *time.Time
}

// Worker polls the stripe_events table and dispatches to billing handlers.
type Worker struct {
	pool     *pgxpool.Pool
	invoicer *Invoicer
	dunning  *Dunning
}

// NewWorker creates a billing Worker.
func NewWorker(pool *pgxpool.Pool) *Worker {
	return &Worker{
		pool:     pool,
		invoicer: NewInvoicer(pool),
		dunning:  NewDunning(pool),
	}
}

// Run starts the billing event poll loop and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[billing] worker started (poll=%s, dunning_tick=%s)",
		billingPollInterval, dunningTickInterval)

	pollTicker := time.NewTicker(billingPollInterval)
	dunningTicker := time.NewTicker(dunningTickInterval)
	defer pollTicker.Stop()
	defer dunningTicker.Stop()

	for {
		select {
		case <-pollTicker.C:
			w.processEvents(ctx)
		case <-dunningTicker.C:
			w.dunning.Advance(ctx)
		case <-ctx.Done():
			log.Printf("[billing] worker shutting down")
			return
		}
	}
}

// processEvents fetches unprocessed stripe events and handles each one.
func (w *Worker) processEvents(ctx context.Context) {
	rows, err := w.pool.Query(ctx, `
		SELECT id::text, event_type, COALESCE(payload,'{}')
		FROM stripe_events
		WHERE processed_at IS NULL
		ORDER BY created_at ASC
		LIMIT 50
	`)
	if err != nil {
		log.Printf("[billing] poll query error: %v", err)
		return
	}
	defer rows.Close()

	var events []stripeEvent
	for rows.Next() {
		var e stripeEvent
		if err := rows.Scan(&e.ID, &e.Type, &e.Data); err != nil {
			log.Printf("[billing] scan error: %v", err)
			continue
		}
		events = append(events, e)
	}
	rows.Close()

	for _, e := range events {
		// Mark as processed BEFORE handling — optimistic lock.  If the handler
		// panics the event won't be retried indefinitely; it can be reprocessed
		// manually via admin tooling if needed.
		if claimed := w.claimEvent(ctx, e.ID); !claimed {
			continue
		}
		w.handleEvent(ctx, e)
	}
}

// claimEvent sets processed_at on the event so another worker won't re-process it.
// Returns true only if this worker claimed it (idempotent via WHERE clause).
func (w *Worker) claimEvent(ctx context.Context, eventID string) bool {
	tag, err := w.pool.Exec(ctx, `
		UPDATE stripe_events
		SET processed_at=NOW()
		WHERE id=$1 AND processed_at IS NULL
	`, eventID)
	if err != nil {
		log.Printf("[billing] claim event error id=%s: %v", eventID, err)
		return false
	}
	return tag.RowsAffected() == 1
}

// handleEvent dispatches one stripe event to the appropriate handler.
func (w *Worker) handleEvent(ctx context.Context, e stripeEvent) {
	log.Printf("[billing] processing event id=%s type=%s", e.ID, e.Type)

	// The stripe event payload is {"type":"...", "data":{"object":{...}}}
	// We need to extract data.object for the handlers.
	var wrapper struct {
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(e.Data, &wrapper); err != nil {
		log.Printf("[billing] parse event wrapper id=%s: %v", e.ID, err)
		return
	}
	obj := wrapper.Data.Object

	switch e.Type {
	case "customer.subscription.updated":
		w.handleSubscriptionUpdated(ctx, obj)

	case "customer.subscription.deleted":
		w.handleSubscriptionDeleted(ctx, obj)

	case "invoice.payment_succeeded":
		if err := w.invoicer.HandlePaymentSucceeded(ctx, obj); err != nil {
			log.Printf("[billing] payment_succeeded error id=%s: %v", e.ID, err)
		}

	case "invoice.payment_failed":
		w.handlePaymentFailed(ctx, obj)

	case "charge.dispute.created":
		w.handleDisputeCreated(ctx, obj)

	default:
		// Unknown event type — logged but not an error.
		log.Printf("[billing] unhandled event type=%s", e.Type)
	}
}

// handleSubscriptionUpdated syncs the subscription status/plan from Stripe.
func (w *Worker) handleSubscriptionUpdated(ctx context.Context, obj json.RawMessage) {
	var s struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Plan   struct {
			Nickname string `json:"nickname"`
		} `json:"plan"`
		CurrentPeriodStart int64 `json:"current_period_start"`
		CurrentPeriodEnd   int64 `json:"current_period_end"`
	}
	if err := json.Unmarshal(obj, &s); err != nil {
		log.Printf("[billing] subscription.updated parse error: %v", err)
		return
	}

	start := time.Unix(s.CurrentPeriodStart, 0).UTC()
	end := time.Unix(s.CurrentPeriodEnd, 0).UTC()

	if _, err := w.pool.Exec(ctx, `
		UPDATE subscriptions
		SET status=$2, plan=COALESCE(NULLIF($3,''), plan),
		    current_period_start=$4, current_period_end=$5,
		    updated_at=NOW()
		WHERE stripe_subscription_id=$1
	`, s.ID, s.Status, s.Plan.Nickname, start, end); err != nil {
		log.Printf("[billing] subscription.updated DB error sub=%s: %v", s.ID, err)
	}
}

// handleSubscriptionDeleted marks the subscription cancelled.
func (w *Worker) handleSubscriptionDeleted(ctx context.Context, obj json.RawMessage) {
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(obj, &s); err != nil {
		log.Printf("[billing] subscription.deleted parse error: %v", err)
		return
	}

	if _, err := w.pool.Exec(ctx, `
		UPDATE subscriptions
		SET status='cancelled', cancelled_at=NOW(), updated_at=NOW()
		WHERE stripe_subscription_id=$1
	`, s.ID); err != nil {
		log.Printf("[billing] subscription.deleted DB error sub=%s: %v", s.ID, err)
	}
}

// handlePaymentFailed initiates or advances dunning for a failed invoice.
func (w *Worker) handlePaymentFailed(ctx context.Context, obj json.RawMessage) {
	var inv struct {
		Subscription string `json:"subscription"`
	}
	if err := json.Unmarshal(obj, &inv); err != nil {
		log.Printf("[billing] payment_failed parse error: %v", err)
		return
	}
	if err := w.dunning.HandlePaymentFailed(ctx, inv.Subscription); err != nil {
		log.Printf("[billing] dunning init error sub=%s: %v", inv.Subscription, err)
	}
}

// handleDisputeCreated flags the account for manual review.
func (w *Worker) handleDisputeCreated(ctx context.Context, obj json.RawMessage) {
	var d struct {
		ID             string `json:"id"`
		PaymentIntent  string `json:"payment_intent"`
		Amount         int    `json:"amount"`
		Currency       string `json:"currency"`
	}
	if err := json.Unmarshal(obj, &d); err != nil {
		log.Printf("[billing] dispute.created parse error: %v", err)
		return
	}

	// Insert an audit log entry for admin review.
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO audit_logs
			(id, action, target_type, target_id, metadata)
		VALUES (gen_random_uuid(), 'dispute_created', 'payment_intent', $1,
		        jsonb_build_object(
					'dispute_id', $2,
					'amount', $3,
					'currency', $4
				))
	`, d.PaymentIntent, d.ID, d.Amount, d.Currency); err != nil {
		log.Printf("[billing] dispute audit log error: %v", err)
	}

	log.Printf("[billing] dispute flagged id=%s amount=%d%s", d.ID, d.Amount, d.Currency)
}
