// Package health provides the site health monitoring worker for DNSFox v2.
// checker.go performs HTTP GET probes against live sites and writes uptime_checks.
package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// probeTimeout is the HTTP timeout for each uptime probe.
	probeTimeout = 10 * time.Second

	// maxRedirects caps how many HTTP redirects the probe will follow.
	maxRedirects = 3
)

// ProbeResult holds the outcome of a single HTTP probe.
type ProbeResult struct {
	SiteID     string
	Domain     string
	LatencyMS  int
	StatusCode int
	Up         bool
	Err        error
}

// Checker issues HTTP probes and persists results.
type Checker struct {
	pool   *pgxpool.Pool
	client *http.Client
}

// NewChecker returns a Checker with a redirect-limited HTTP client.
func NewChecker(pool *pgxpool.Pool) *Checker {
	redirectCount := 0
	client := &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			redirectCount++
			if redirectCount > maxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return &Checker{pool: pool, client: client}
}

// Probe issues an HTTP GET to https://{domain}/ and returns the result.
// A 2xx status code is treated as success.
func (c *Checker) Probe(ctx context.Context, siteID, domain string) ProbeResult {
	start := time.Now()
	res := ProbeResult{SiteID: siteID, Domain: domain}

	url := "https://" + domain + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.Err = fmt.Errorf("build request: %w", err)
		res.LatencyMS = int(time.Since(start).Milliseconds())
		if err2 := c.persist(ctx, res, "down"); err2 != nil {
			_ = err2 // log handled by caller
		}
		return res
	}

	resp, err := c.client.Do(req)
	res.LatencyMS = int(time.Since(start).Milliseconds())
	if err != nil {
		res.Err = err
		_ = c.persist(ctx, res, "down")
		return res
	}
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode
	// 2xx codes are healthy; any other code is treated as down.
	res.Up = resp.StatusCode >= 200 && resp.StatusCode < 300
	status := "up"
	if !res.Up {
		status = "down"
	}
	_ = c.persist(ctx, res, status)
	return res
}

// persist writes one uptime_checks row.
func (c *Checker) persist(ctx context.Context, r ProbeResult, status string) error {
	errMsg := ""
	if r.Err != nil {
		errMsg = r.Err.Error()
	}
	_, err := c.pool.Exec(ctx, `
		INSERT INTO uptime_checks
			(id, instance_id, status, response_ms, status_code, error_message)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
	`, r.SiteID, status, r.LatencyMS, r.StatusCode,
		func() interface{} {
			if errMsg == "" {
				return nil
			}
			return errMsg
		}(),
	)
	return err
}
