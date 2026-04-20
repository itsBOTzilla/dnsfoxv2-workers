// Package billing provides billing event processing workers for DNSFox v2.
// invoicer.go handles invoice.payment_succeeded events: it creates an invoice
// record in the DB and resets any dunning state.
package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Invoicer creates invoice DB records from Stripe payment_succeeded events.
type Invoicer struct {
	pool *pgxpool.Pool
}

// NewInvoicer returns an Invoicer.
func NewInvoicer(pool *pgxpool.Pool) *Invoicer {
	return &Invoicer{pool: pool}
}

// HandlePaymentSucceeded creates an invoice record and clears dunning state.
// stripeData is the raw JSON payload of the Stripe invoice object.
func (inv *Invoicer) HandlePaymentSucceeded(ctx context.Context, stripeData json.RawMessage) error {
	var si struct {
		ID                 string `json:"id"`
		CustomerID         string `json:"customer"`
		SubscriptionID     string `json:"subscription"`
		AmountPaid         int    `json:"amount_paid"`
		Currency           string `json:"currency"`
		Status             string `json:"status"`
		HostedInvoiceURL   string `json:"hosted_invoice_url"`
		InvoicePDF         string `json:"invoice_pdf"`
		PeriodStart        int64  `json:"period_start"`
		PeriodEnd          int64  `json:"period_end"`
	}
	if err := json.Unmarshal(stripeData, &si); err != nil {
		return fmt.Errorf("invoicer: unmarshal stripe invoice: %w", err)
	}

	periodStart := time.Unix(si.PeriodStart, 0).UTC()
	periodEnd := time.Unix(si.PeriodEnd, 0).UTC()
	paidAt := time.Now().UTC()

	// Upsert the invoice row: if we already recorded it from a webhook,
	// update status and paid_at rather than inserting a duplicate.
	_, err := inv.pool.Exec(ctx, `
		INSERT INTO invoices
			(id, user_id, subscription_id, stripe_invoice_id,
			 amount_cents, currency, status, paid_at,
			 hosted_invoice_url, pdf_url,
			 period_start, period_end)
		SELECT
			gen_random_uuid(),
			s.user_id,
			s.id,
			$1,
			$2, $3, 'paid', $4,
			$5, $6,
			$7, $8
		FROM subscriptions s
		WHERE s.stripe_subscription_id = $9
		ON CONFLICT (stripe_invoice_id)
		DO UPDATE SET
			status  = 'paid',
			paid_at = EXCLUDED.paid_at,
			updated_at = NOW()
	`, si.ID, si.AmountPaid, si.Currency, paidAt,
		si.HostedInvoiceURL, si.InvoicePDF,
		periodStart, periodEnd,
		si.SubscriptionID,
	)
	if err != nil {
		return fmt.Errorf("invoicer: upsert invoice: %w", err)
	}

	// Reset dunning: if the subscription was in any dunning stage, clear it.
	tag, err := inv.pool.Exec(ctx, `
		UPDATE subscriptions
		SET dunning_stage         = 'none',
		    dunning_started_at    = NULL,
		    dunning_next_action_at = NULL,
		    status                = 'active',
		    updated_at            = NOW()
		WHERE stripe_subscription_id = $1
		  AND dunning_stage != 'none'
	`, si.SubscriptionID)
	if err != nil {
		log.Printf("[billing/invoicer] dunning reset error sub=%s: %v", si.SubscriptionID, err)
	} else if tag.RowsAffected() > 0 {
		log.Printf("[billing/invoicer] dunning cleared for sub=%s after payment", si.SubscriptionID)
	}

	log.Printf("[billing/invoicer] invoice recorded stripe=%s amount=%d%s",
		si.ID, si.AmountPaid, si.Currency)
	return nil
}

// CalculateProration returns the prorated credit/charge in cents for a
// mid-cycle plan change.  This is informational — Stripe handles the actual
// proration charge.  Returned value is positive when the new plan costs more.
//
// Formula: (remaining_days / total_days) * price_difference_cents
func CalculateProration(periodStart, periodEnd time.Time, oldPriceCents, newPriceCents int) int {
	total := periodEnd.Sub(periodStart)
	remaining := time.Until(periodEnd)
	if remaining < 0 {
		remaining = 0
	}
	if total <= 0 {
		return 0
	}

	fraction := float64(remaining) / float64(total)
	diff := float64(newPriceCents - oldPriceCents)
	return int(fraction * diff)
}
