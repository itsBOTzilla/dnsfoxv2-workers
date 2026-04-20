// Package backup provides the backup scheduling and pruning worker for DNSFox v2.
// pruner.go enforces plan-based backup retention: it deletes expired backup
// records from the DB and removes the associated files from Backblaze B2.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// planRetention maps plan names to how long backups are kept.
var planRetention = map[string]time.Duration{
	"fox":   7 * 24 * time.Hour,
	"swift": 14 * 24 * time.Hour,
	"apex":  30 * 24 * time.Hour,
	"titan": 60 * 24 * time.Hour,
}

const (
	// b2DeleteURL is the Backblaze B2 API endpoint for file deletion.
	b2DeleteURL = "https://api.backblazeb2.com/b2api/v2/b2_delete_file_version"

	// b2AuthURL is the endpoint used to obtain an authorization token.
	b2AuthURL = "https://api.backblazeb2.com/b2api/v2/b2_authorize_account"

	// b2MaxRetries is the max number of times we retry a failed B2 API call.
	b2MaxRetries = 3
)

// expiredBackup is a row projection used during pruning.
type expiredBackup struct {
	ID         string
	InstanceID string
	Plan       string
	B2FileID   string // populated when the backup lives in B2
	B2FileName string
}

// Pruner deletes expired backups from DB and B2.
type Pruner struct {
	pool       *pgxpool.Pool
	httpClient *http.Client
}

// NewPruner returns a Pruner.
func NewPruner(pool *pgxpool.Pool) *Pruner {
	return &Pruner{
		pool:       pool,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// PruneExpired finds and deletes all backups that exceed plan retention,
// removing B2 objects when b2_file_id is populated.
func (p *Pruner) PruneExpired(ctx context.Context) {
	rows, err := p.pool.Query(ctx, `
		SELECT b.id::text,
		       COALESCE(b.instance_id::text,''),
		       COALESCE(i.plan,'fox'),
		       COALESCE(b.b2_file_id,''),
		       COALESCE(b.b2_file_name,'')
		FROM backups b
		LEFT JOIN instances i ON i.id = b.instance_id
		WHERE b.deleted_at IS NULL
		  AND b.status = 'completed'
		  AND b.created_at < NOW() - CASE COALESCE(i.plan,'fox')
			  WHEN 'titan' THEN INTERVAL '60 days'
			  WHEN 'apex'  THEN INTERVAL '30 days'
			  WHEN 'swift' THEN INTERVAL '14 days'
			  ELSE              INTERVAL '7 days'
		  END
	`)
	if err != nil {
		log.Printf("[backup/pruner] query error: %v", err)
		return
	}
	defer rows.Close()

	var expired []expiredBackup
	for rows.Next() {
		var b expiredBackup
		if err := rows.Scan(&b.ID, &b.InstanceID, &b.Plan,
			&b.B2FileID, &b.B2FileName); err != nil {
			log.Printf("[backup/pruner] scan error: %v", err)
			continue
		}
		expired = append(expired, b)
	}
	rows.Close()

	if len(expired) == 0 {
		return
	}

	// Obtain a B2 auth token once for the entire prune run.
	var b2Token string
	if len(expired) > 0 {
		var tokenErr error
		b2Token, tokenErr = p.b2Authorize(ctx)
		if tokenErr != nil {
			log.Printf("[backup/pruner] B2 auth error: %v", tokenErr)
			// Continue without B2 deletion — DB records will still be pruned.
		}
	}

	for _, b := range expired {
		// Delete the B2 object before removing the DB record so we never lose
		// the reference before the file is gone.
		if b.B2FileID != "" && b2Token != "" {
			if err := p.b2DeleteWithRetry(ctx, b2Token, b.B2FileID, b.B2FileName); err != nil {
				log.Printf("[backup/pruner] B2 delete failed backup=%s: %v", b.ID, err)
				// Log but continue — the DB soft-delete below is still performed.
			}
		}

		// Soft-delete the DB record.
		if _, err := p.pool.Exec(ctx, `
			UPDATE backups SET deleted_at=NOW() WHERE id=$1
		`, b.ID); err != nil {
			log.Printf("[backup/pruner] DB delete error backup=%s: %v", b.ID, err)
			continue
		}
		log.Printf("[backup/pruner] pruned backup=%s plan=%s b2=%s", b.ID, b.Plan, b.B2FileID)
	}

	log.Printf("[backup/pruner] pruned %d expired backups", len(expired))
}

// b2Authorize obtains a Backblaze B2 authorization token using env-var credentials.
// Returns the authorization token string.
func (p *Pruner) b2Authorize(ctx context.Context) (string, error) {
	keyID := os.Getenv("WARDEN_B2_APPLICATION_KEY_ID")
	appKey := os.Getenv("WARDEN_B2_APPLICATION_KEY")
	if keyID == "" || appKey == "" {
		return "", fmt.Errorf("b2: WARDEN_B2_APPLICATION_KEY_ID or WARDEN_B2_APPLICATION_KEY not set")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b2AuthURL, nil)
	if err != nil {
		return "", fmt.Errorf("b2: build auth request: %w", err)
	}
	req.SetBasicAuth(keyID, appKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("b2: auth request: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		AuthorizationToken string `json:"authorizationToken"`
		Allowed            struct {
			Capabilities []string `json:"capabilities"`
		} `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("b2: decode auth response: %w", err)
	}
	if body.AuthorizationToken == "" {
		return "", fmt.Errorf("b2: empty authorization token")
	}
	return body.AuthorizationToken, nil
}

// b2DeleteWithRetry calls b2_delete_file_version with exponential back-off.
func (p *Pruner) b2DeleteWithRetry(ctx context.Context, token, fileID, fileName string) error {
	payload := map[string]string{
		"fileId":   fileID,
		"fileName": fileName,
	}

	var lastErr error
	for attempt := 1; attempt <= b2MaxRetries; attempt++ {
		if err := p.b2Delete(ctx, token, payload); err != nil {
			lastErr = err
			// Exponential back-off: 2^attempt seconds (2s, 4s, 8s).
			wait := time.Duration(1<<attempt) * time.Second
			log.Printf("[backup/pruner] B2 delete attempt %d/%d failed, retry in %s: %v",
				attempt, b2MaxRetries, wait, err)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("b2 delete exhausted retries: %w", lastErr)
}

// b2Delete performs a single B2 deleteFileVersion HTTP call.
func (p *Pruner) b2Delete(ctx context.Context, token string, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b2DeleteURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("b2 API status %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}
