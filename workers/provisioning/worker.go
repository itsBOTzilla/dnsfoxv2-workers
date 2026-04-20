// Package provisioning provides the provisioning job worker for DNSFox v2.
// worker.go polls provisioning_jobs for pending work, claims jobs atomically,
// dispatches them to the correct Warden v2 server via Connect-Go gRPC, and
// records completion or failure with retry logic.
package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	wardenv1 "github.com/itsBOTzilla/dnsfoxv2-proto/gen/go/warden/v1"
	"github.com/itsBOTzilla/dnsfoxv2-proto/gen/go/warden/v1/wardenv1connect"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// jobTimeout is the maximum time a single provisioning job may run before
	// it is forcibly cancelled and marked failed.
	jobTimeout = 30 * time.Minute

	// maxRetries is the maximum number of times a job will be retried before
	// it is permanently marked failed.
	maxRetries = 3

	// maxConcurrentPerServer caps how many jobs can run in parallel against a
	// single Warden server, preventing agent overload.
	maxConcurrentPerServer = 4

	// pollInterval is how often the worker polls for new pending jobs.
	pollInterval = 10 * time.Second
)

// serverSemaphores holds a per-server buffered channel acting as a semaphore.
var (
	semMu      sync.Mutex
	serverSems = map[string]chan struct{}{}
)

// acquireSem acquires a slot on the per-server semaphore, blocking until
// capacity is available or ctx is cancelled.
func acquireSem(ctx context.Context, serverID string) error {
	semMu.Lock()
	if _, ok := serverSems[serverID]; !ok {
		serverSems[serverID] = make(chan struct{}, maxConcurrentPerServer)
	}
	ch := serverSems[serverID]
	semMu.Unlock()

	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSem releases the per-server semaphore slot.
func releaseSem(serverID string) {
	semMu.Lock()
	ch := serverSems[serverID]
	semMu.Unlock()
	<-ch
}

// provisioningJob is a projection of one provisioning_jobs row.
type provisioningJob struct {
	ID           string
	InstanceID   string
	ServerID     string
	JobType      string
	Status       string
	Attempt      int
	ErrorMessage string
}

// Worker polls the provisioning_jobs queue and dispatches work to Warden agents.
type Worker struct {
	pool      *pgxpool.Pool
	masterKey []byte // AES master key for payload encryption (32 bytes)
}

// NewWorker creates a new provisioning Worker.
// masterKey must be exactly 32 bytes for AES-256.
func NewWorker(pool *pgxpool.Pool, masterKey []byte) *Worker {
	return &Worker{pool: pool, masterKey: masterKey}
}

// Run starts the poll-dispatch loop.  It runs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[provisioning] worker started (poll=%s, timeout=%s)", pollInterval, jobTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.poll(ctx)
		case <-ctx.Done():
			log.Printf("[provisioning] worker shutting down")
			return
		}
	}
}

// poll fetches pending jobs and spawns a goroutine per job.
func (w *Worker) poll(ctx context.Context) {
	rows, err := w.pool.Query(ctx, `
		SELECT id::text, instance_id::text, server_id::text,
		       job_type, status, attempt, COALESCE(error_message,'')
		FROM provisioning_jobs
		WHERE status = 'pending'
		  AND (retry_after IS NULL OR retry_after <= NOW())
		ORDER BY priority DESC, created_at ASC
		LIMIT 20
	`)
	if err != nil {
		log.Printf("[provisioning] poll query error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var j provisioningJob
		if err := rows.Scan(&j.ID, &j.InstanceID, &j.ServerID,
			&j.JobType, &j.Status, &j.Attempt, &j.ErrorMessage); err != nil {
			log.Printf("[provisioning] scan error: %v", err)
			continue
		}
		// Claim atomically before launching goroutine — if another worker
		// already claimed it the UPDATE affects 0 rows and we skip.
		claimed, err := w.claimJob(ctx, j.ID)
		if err != nil {
			log.Printf("[provisioning] claim error job=%s: %v", j.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		go w.dispatch(ctx, j)
	}
}

// claimJob atomically transitions a job from pending to claimed.
// Returns true only when this worker wins the race.
func (w *Worker) claimJob(ctx context.Context, jobID string) (bool, error) {
	workerID := uuid.New().String()
	tag, err := w.pool.Exec(ctx, `
		UPDATE provisioning_jobs
		SET status='claimed', claimed_at=NOW(), worker_id=$2,
		    attempt=attempt+1, updated_at=NOW()
		WHERE id=$1 AND status='pending'
	`, jobID, workerID)
	if err != nil {
		return false, fmt.Errorf("provisioning: claim: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// dispatch runs a single provisioning job inside a 30-minute timeout.
func (w *Worker) dispatch(parentCtx context.Context, j provisioningJob) {
	// Acquire per-server concurrency slot before making any gRPC call.
	if err := acquireSem(parentCtx, j.ServerID); err != nil {
		log.Printf("[provisioning] semaphore acquire cancelled job=%s", j.ID)
		return
	}
	defer releaseSem(j.ServerID)

	ctx, cancel := context.WithTimeout(parentCtx, jobTimeout)
	defer cancel()

	log.Printf("[provisioning] dispatching job=%s type=%s server=%s", j.ID, j.JobType, j.ServerID)

	if err := w.execute(ctx, j); err != nil {
		log.Printf("[provisioning] job=%s failed (attempt=%d): %v", j.ID, j.Attempt, err)
		w.markFailed(parentCtx, j, err.Error())
		return
	}

	if err := w.markCompleted(parentCtx, j.ID); err != nil {
		log.Printf("[provisioning] markCompleted job=%s: %v", j.ID, err)
	}
}

// execute resolves the Warden URL, encrypts credentials, and calls the
// appropriate gRPC RPC based on job_type.
func (w *Worker) execute(ctx context.Context, j provisioningJob) error {
	var ipAddress string
	var wardenPort int
	err := w.pool.QueryRow(ctx,
		`SELECT ip_address, COALESCE(warden_v2_port,9202) FROM servers WHERE id=$1`,
		j.ServerID,
	).Scan(&ipAddress, &wardenPort)
	if err != nil {
		return fmt.Errorf("look up server: %w", err)
	}

	// Load the raw JSONB payload stored by the API.
	var payloadStr string
	if err := w.pool.QueryRow(ctx,
		`SELECT COALESCE(payload::text,'{}') FROM provisioning_jobs WHERE id=$1`, j.ID,
	).Scan(&payloadStr); err != nil {
		return fmt.Errorf("load payload: %w", err)
	}

	// Encrypt credentials with a server-specific derived key so leaked
	// ciphertext for one server cannot be decrypted by another server's key.
	encryptedCreds, err := EncryptPayload(w.masterKey, j.ServerID, []byte(payloadStr))
	if err != nil {
		return fmt.Errorf("encrypt payload: %w", err)
	}

	baseURL := fmt.Sprintf("http://%s:%d", ipAddress, wardenPort)
	client := wardenv1connect.NewWardenServiceClient(
		&http.Client{Timeout: jobTimeout},
		baseURL,
	)

	switch j.JobType {
	case "provision_wordpress", "provision_php", "provision_nodejs":
		return w.callProvisionSite(ctx, client, j, encryptedCreds, payloadStr)
	case "deprovision_site":
		return w.callDeprovisionSite(ctx, client, j, payloadStr)
	case "purge_cache":
		return w.callPurgeCache(ctx, client, j, payloadStr)
	default:
		return fmt.Errorf("unknown job_type: %s", j.JobType)
	}
}

// callProvisionSite sends a ProvisionSite RPC to the Warden agent.
func (w *Worker) callProvisionSite(
	ctx context.Context,
	client wardenv1connect.WardenServiceClient,
	j provisioningJob,
	encryptedCreds []byte,
	rawPayload string,
) error {
	var p struct {
		Domain     string `json:"domain"`
		AppType    string `json:"app_type"`
		PHPVersion string `json:"php_version"`
		Plan       string `json:"plan"`
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
		return fmt.Errorf("parse payload: %w", err)
	}

	req := connect.NewRequest(&wardenv1.ProvisionSiteRequest{
		JobId:                j.ID,
		SiteId:               j.InstanceID,
		Domain:               p.Domain,
		Type:                 p.AppType,
		PhpVersion:           p.PHPVersion,
		Plan:                 p.Plan,
		CustomerId:           p.CustomerID,
		EncryptedCredentials: encryptedCreds,
	})

	resp, err := client.ProvisionSite(ctx, req)
	if err != nil {
		return fmt.Errorf("ProvisionSite rpc: %w", err)
	}
	// DONE is the success terminal state from the agent.
	if resp.Msg.GetStatus() == wardenv1.ProvisioningStatus_PROVISIONING_STATUS_FAILED {
		return fmt.Errorf("ProvisionSite: agent reported failure")
	}
	return nil
}

// callDeprovisionSite sends a DeprovisionSite RPC.
func (w *Worker) callDeprovisionSite(
	ctx context.Context,
	client wardenv1connect.WardenServiceClient,
	j provisioningJob,
	rawPayload string,
) error {
	req := connect.NewRequest(&wardenv1.DeprovisionSiteRequest{
		SiteId: j.InstanceID,
		JobId:  j.ID,
	})
	_, err := client.DeprovisionSite(ctx, req)
	if err != nil {
		return fmt.Errorf("DeprovisionSite rpc: %w", err)
	}
	return nil
}

// callPurgeCache sends a PurgeSiteCache RPC.
func (w *Worker) callPurgeCache(
	ctx context.Context,
	client wardenv1connect.WardenServiceClient,
	j provisioningJob,
	rawPayload string,
) error {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
		return fmt.Errorf("parse payload: %w", err)
	}

	req := connect.NewRequest(&wardenv1.PurgeSiteCacheRequest{
		SiteId: j.InstanceID,
		Url:    p.URL,
	})
	resp, err := client.PurgeSiteCache(ctx, req)
	if err != nil {
		return fmt.Errorf("PurgeSiteCache rpc: %w", err)
	}
	if !resp.Msg.GetSuccess() {
		return fmt.Errorf("PurgeSiteCache: agent returned failure")
	}
	return nil
}

// markCompleted sets job status to completed.
func (w *Worker) markCompleted(ctx context.Context, jobID string) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE provisioning_jobs
		SET status='completed', completed_at=NOW(), updated_at=NOW()
		WHERE id=$1
	`, jobID)
	return err
}

// markFailed schedules a retry (if under maxRetries) using exponential back-off
// (attempt² × 60s), or permanently marks the job and its instance failed.
func (w *Worker) markFailed(ctx context.Context, j provisioningJob, errMsg string) {
	if j.Attempt < maxRetries {
		backoff := time.Duration(j.Attempt*j.Attempt) * 60 * time.Second
		retryAt := time.Now().Add(backoff)
		if _, err := w.pool.Exec(ctx, `
			UPDATE provisioning_jobs
			SET status='pending', error_message=$2, retry_after=$3, updated_at=NOW()
			WHERE id=$1
		`, j.ID, errMsg, retryAt); err != nil {
			log.Printf("[provisioning] markFailed retry error job=%s: %v", j.ID, err)
		}
		log.Printf("[provisioning] job=%s scheduled retry at %s (attempt %d/%d)",
			j.ID, retryAt.Format(time.RFC3339), j.Attempt, maxRetries)
		return
	}

	// Terminal failure — update job and surface the error on the instance.
	if _, err := w.pool.Exec(ctx, `
		UPDATE provisioning_jobs
		SET status='failed', error_message=$2, completed_at=NOW(), updated_at=NOW()
		WHERE id=$1
	`, j.ID, errMsg); err != nil {
		log.Printf("[provisioning] markFailed terminal job=%s: %v", j.ID, err)
	}
	if _, err := w.pool.Exec(ctx, `
		UPDATE instances
		SET status='failed', error_message=$2, updated_at=NOW()
		WHERE id=$1
	`, j.InstanceID, errMsg); err != nil {
		log.Printf("[provisioning] instance update failed job=%s: %v", j.ID, err)
	}
}
