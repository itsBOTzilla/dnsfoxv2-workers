// Package billing provides billing event processing workers for DNSFox v2.
// dunning.go implements the multi-stage dunning flow triggered by
// invoice.payment_failed events.  Each stage is idempotent: we check the
// current dunning_step before advancing to avoid double-escalation.
package billing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dunningTimeline describes when each step fires (days since first failure).
var dunningTimeline = []struct {
	Day  int
	Step string
	Desc string
}{
	{Day: 1, Step: "warning_1", Desc: "payment failed — warning logged"},
	{Day: 3, Step: "warning_2", Desc: "dunning warning email queued"},
	{Day: 7, Step: "final_warning", Desc: "final dunning warning"},
	{Day: 14, Step: "suspended", Desc: "site(s) suspended, status=past_due"},
}

// subscriptionRow is a minimal projection used by the dunning engine.
type subscriptionRow struct {
	ID                   string
	UserID               string
	StripeSubscriptionID string
	Plan                 string
	DunningStep          string
	FailedAt             time.Time
}

// Dunning implements the staged payment failure escalation workflow.
type Dunning struct {
	pool *pgxpool.Pool
}

// NewDunning returns a Dunning engine.
func NewDunning(pool *pgxpool.Pool) *Dunning {
	return &Dunning{pool: pool}
}

// HandlePaymentFailed initialises dunning for a subscription that just failed.
// It is safe to call multiple times — subsequent calls are no-ops if dunning
// is already active for this subscription.
func (d *Dunning) HandlePaymentFailed(ctx context.Context, stripeSubscriptionID string) error {
	// Only start dunning if the subscription is not already in a dunning stage.
	tag, err := d.pool.Exec(ctx, `
		UPDATE subscriptions
		SET dunning_stage          = 'warning_1',
		    dunning_started_at     = NOW(),
		    dunning_next_action_at = NOW() + INTERVAL '3 days',
		    updated_at             = NOW()
		WHERE stripe_subscription_id = $1
		  AND (dunning_stage = 'none' OR dunning_stage IS NULL)
	`, stripeSubscriptionID)
	if err != nil {
		return fmt.Errorf("dunning: init: %w", err)
	}
	if tag.RowsAffected() == 0 {
		log.Printf("[dunning] sub=%s already in dunning or not found", stripeSubscriptionID)
		return nil
	}

	// Audit log entry so admins can see the timeline.
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO audit_logs
			(id, action, target_type, target_id, metadata)
		SELECT gen_random_uuid(), 'dunning_started', 'subscription', id::text,
		       jsonb_build_object('stripe_sub_id', $1, 'day', 1)
		FROM subscriptions WHERE stripe_subscription_id = $1
	`, stripeSubscriptionID); err != nil {
		log.Printf("[dunning] audit log error: %v", err)
	}

	log.Printf("[dunning] dunning started sub=%s", stripeSubscriptionID)
	return nil
}

// Advance runs the periodic dunning escalation pass.  It should be called
// every time the billing worker ticks.  Each subscription in an active dunning
// stage is examined; if dunning_next_action_at has passed we advance the stage.
func (d *Dunning) Advance(ctx context.Context) {
	rows, err := d.pool.Query(ctx, `
		SELECT id::text, user_id::text,
		       COALESCE(stripe_subscription_id,''),
		       plan, dunning_stage,
		       COALESCE(dunning_started_at, NOW())
		FROM subscriptions
		WHERE dunning_stage NOT IN ('none','canceled','suspended')
		  AND dunning_next_action_at IS NOT NULL
		  AND dunning_next_action_at <= NOW()
	`)
	if err != nil {
		log.Printf("[dunning] advance query error: %v", err)
		return
	}
	defer rows.Close()

	var subs []subscriptionRow
	for rows.Next() {
		var s subscriptionRow
		if err := rows.Scan(&s.ID, &s.UserID, &s.StripeSubscriptionID,
			&s.Plan, &s.DunningStep, &s.FailedAt); err != nil {
			log.Printf("[dunning] scan error: %v", err)
			continue
		}
		subs = append(subs, s)
	}
	rows.Close()

	for _, s := range subs {
		if err := d.escalate(ctx, s); err != nil {
			log.Printf("[dunning] escalate error sub=%s: %v", s.ID, err)
		}
	}
}

// escalate advances a subscription to the next dunning stage.
func (d *Dunning) escalate(ctx context.Context, s subscriptionRow) error {
	daysSince := int(time.Since(s.FailedAt).Hours() / 24)

	switch s.DunningStep {
	case "warning_1":
		// Advance to warning_2 (day 3).
		return d.advance(ctx, s.ID, "warning_2",
			3*24*time.Hour,
			fmt.Sprintf("dunning_warning day=%d", daysSince))

	case "warning_2":
		// Advance to final_warning (day 7).
		return d.advance(ctx, s.ID, "final_warning",
			7*24*time.Hour,
			fmt.Sprintf("dunning_final_warning day=%d", daysSince))

	case "final_warning":
		// Suspend at day 14: mark subscription past_due and suspend instances.
		if err := d.suspend(ctx, s); err != nil {
			return err
		}
		return d.advance(ctx, s.ID, "suspended",
			0, // no further escalation
			fmt.Sprintf("dunning_suspended day=%d", daysSince))
	}

	return nil
}

// advance sets the next dunning_stage and schedules the following action.
// nextActionIn == 0 means no further action (terminal stage).
func (d *Dunning) advance(ctx context.Context, subID, nextStage string,
	nextActionIn time.Duration, logAction string) error {

	var nextAction interface{}
	if nextActionIn > 0 {
		t := time.Now().Add(nextActionIn)
		nextAction = t
	}

	_, err := d.pool.Exec(ctx, `
		UPDATE subscriptions
		SET dunning_stage          = $2,
		    dunning_next_action_at = $3,
		    updated_at             = NOW()
		WHERE id = $1
	`, subID, nextStage, nextAction)
	if err != nil {
		return fmt.Errorf("dunning: advance update: %w", err)
	}

	if _, err := d.pool.Exec(ctx, `
		INSERT INTO audit_logs
			(id, action, target_type, target_id, metadata)
		VALUES (gen_random_uuid(), $2, 'subscription', $1,
		        jsonb_build_object('stage', $3))
	`, subID, logAction, nextStage); err != nil {
		log.Printf("[dunning] audit log error sub=%s: %v", subID, err)
	}

	log.Printf("[dunning] sub=%s advanced to stage=%s", subID, nextStage)
	return nil
}

// suspend marks subscription status=past_due and suspends all customer sites.
func (d *Dunning) suspend(ctx context.Context, s subscriptionRow) error {
	// Mark subscription past_due.
	if _, err := d.pool.Exec(ctx, `
		UPDATE subscriptions
		SET status='past_due', updated_at=NOW()
		WHERE id=$1
	`, s.ID); err != nil {
		return fmt.Errorf("dunning: mark past_due: %w", err)
	}

	// Suspend all non-deleted instances owned by this user.
	tag, err := d.pool.Exec(ctx, `
		UPDATE instances
		SET status='suspended', updated_at=NOW()
		WHERE customer_id=$1 AND status NOT IN ('deleted','suspended')
	`, s.UserID)
	if err != nil {
		return fmt.Errorf("dunning: suspend instances: %w", err)
	}

	log.Printf("[dunning] suspended %d instances for user=%s", tag.RowsAffected(), s.UserID)
	return nil
}
