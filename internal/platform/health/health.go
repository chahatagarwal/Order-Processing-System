// Package health implements Kubernetes-style liveness and readiness probes.
//
// ============================================================================
// THE DISTINCTION THAT MATTERS (and that interviewers ask about)
// ============================================================================
//
// /healthz = LIVENESS — "is this process fundamentally broken?"
//
//	Answer by returning 200 and NOTHING ELSE. It must not touch the database,
//	the cache, or any dependency.
//
//	WHY: Kubernetes RESTARTS a pod that fails liveness. Suppose Postgres has a
//	30-second blip. If /healthz checked the database, EVERY replica would fail
//	liveness simultaneously and Kubernetes would kill ALL of them at once.
//	Now, when Postgres recovers, you have no running pods, cold caches, and a
//	stampede of restarts. A 30-second database blip became a 10-minute outage
//	THAT YOUR HEALTH CHECK CAUSED.
//
//	Restarting a process only fixes PROCESS problems (deadlock, exhausted
//	memory, wedged goroutines). So liveness should only detect those.
//
// /readyz = READINESS — "can I serve a real request right now?"
//
//	Answer by checking dependencies: database, cache, downstream services.
//
//	WHY IT IS SAFE TO CHECK DEPENDENCIES HERE: failing readiness only removes
//	the pod from the load balancer's rotation. The process keeps running, keeps
//	retrying, and rejoins automatically when its dependencies return. Nothing
//	is destroyed.
//
// THE ONE-LINE SUMMARY: liveness failure = "kill me". Readiness failure =
// "don't send me traffic yet". Confusing them is how teams cause outages while
// trying to prevent them.
//
// (There is a third kind, a STARTUP probe, for services with slow boots:
// it suppresses liveness checks until the first success, so a 60-second
// migration does not get killed at second 10.)
// ============================================================================
package health

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/httpx"
)

// Check is one named dependency probe.
type Check struct {
	Name string
	// Probe should be CHEAP (a ping, not a query) and must honour ctx.
	Probe func(ctx context.Context) error
	// Critical distinguishes "we cannot work without this" from "degraded".
	//
	// Postgres: Critical=true  — no database, no orders. Serve nothing.
	// Redis:    Critical=false — cache down means SLOW, not BROKEN. Removing
	//           the pod from the LB because the cache is down would take the
	//           whole site offline over an optimisation. This single flag
	//           is the difference between graceful degradation and an outage.
	Critical bool
}

// Checker aggregates checks and serves the probe endpoints.
type Checker struct {
	serviceName string
	version     string
	startedAt   time.Time

	mu     sync.RWMutex
	checks []Check

	// ready is a manual gate. We flip it to true only after migrations have
	// run and workers have started, so Kubernetes does not send traffic to a
	// pod that is still booting.
	ready bool

	// probeTimeout bounds the TOTAL time a readiness check may take. Without
	// it, a hung dependency makes /readyz hang, the kubelet's probe times out
	// ambiguously, and you get flapping instead of a clear signal.
	probeTimeout time.Duration

	// cache holds the last readiness result briefly.
	//
	// WHY: with 3 replicas probed every 5s plus an external monitor plus a
	// load balancer, your database gets a steady stream of probe queries
	// forever. Caching for ~1s collapses that to near-zero cost while still
	// reacting within one probe interval.
	lastResult  *Report
	lastChecked time.Time
	cacheFor    time.Duration
}

// Report is the JSON body of /readyz. It names each dependency and its state,
// so an on-call engineer gets the answer from the probe itself and does not
// have to go hunting through dashboards.
type Report struct {
	Status     string            `json:"status"` // ok | degraded | unavailable
	Service    string            `json:"service"`
	Version    string            `json:"version"`
	UptimeSecs float64           `json:"uptime_seconds"`
	CheckedAt  time.Time         `json:"checked_at"`
	Checks     map[string]Detail `json:"checks"`
}

type Detail struct {
	Status   string  `json:"status"` // ok | fail
	Critical bool    `json:"critical"`
	Error    string  `json:"error,omitempty"`
	TookMs   float64 `json:"took_ms"`
}

func NewChecker(serviceName, version string) *Checker {
	return &Checker{
		serviceName:  serviceName,
		version:      version,
		startedAt:    time.Now(),
		probeTimeout: 2 * time.Second,
		cacheFor:     time.Second,
	}
}

// Register adds a dependency check. Safe to call during boot.
func (c *Checker) Register(check Check) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, check)
}

// SetReady opens or closes the manual readiness gate.
func (c *Checker) SetReady(ready bool) {
	c.mu.Lock()
	c.ready = ready
	c.lastResult = nil // force a fresh evaluation on the next probe
	c.mu.Unlock()
}

// LivenessHandler serves /healthz. Note what it does NOT do: no dependency
// checks, no locks on the hot path, no I/O. If this handler can run, the
// process is alive, and that is the entire question being asked.
func (c *Checker) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"status":         "alive",
			"service":        c.serviceName,
			"uptime_seconds": time.Since(c.startedAt).Seconds(),
		})
	}
}

// ReadinessHandler serves /readyz.
//
// STATUS CODES:
//
//	200 ok         — everything healthy.
//	200 degraded   — a NON-critical dependency is down. We deliberately return
//	                 200 so we keep receiving traffic: a cache outage should
//	                 make us slower, not absent. The body says "degraded" so
//	                 monitoring can alert without the LB pulling us out.
//	503 unavailable— a CRITICAL dependency is down, or we are still booting.
func (c *Checker) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := c.Evaluate(r.Context())
		status := http.StatusOK
		if report.Status == "unavailable" {
			status = http.StatusServiceUnavailable
		}
		// Probe responses must never be cached by an intermediary — a cached
		// "ok" would hide a real outage from the load balancer.
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, status, report)
	}
}

// Evaluate runs all checks (or returns the cached result).
func (c *Checker) Evaluate(ctx context.Context) Report {
	c.mu.RLock()
	if c.lastResult != nil && time.Since(c.lastChecked) < c.cacheFor {
		cached := *c.lastResult
		c.mu.RUnlock()
		return cached
	}
	checks := make([]Check, len(c.checks))
	copy(checks, c.checks)
	ready := c.ready
	c.mu.RUnlock()

	report := Report{
		Status:     "ok",
		Service:    c.serviceName,
		Version:    c.version,
		UptimeSecs: time.Since(c.startedAt).Seconds(),
		CheckedAt:  time.Now().UTC(),
		Checks:     map[string]Detail{},
	}

	if !ready {
		report.Status = "unavailable"
		report.Checks["startup"] = Detail{Status: "fail", Critical: true, Error: "service is still starting up"}
		return report
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.probeTimeout)
	defer cancel()

	// Run all probes CONCURRENTLY. Sequentially, 4 dependencies at 500ms each
	// would make /readyz take 2 seconds and exceed the probe timeout — so a
	// perfectly healthy service would be reported as unready.
	type result struct {
		name   string
		detail Detail
	}
	results := make(chan result, len(checks))
	var wg sync.WaitGroup
	for _, chk := range checks {
		wg.Add(1)
		go func(chk Check) {
			defer wg.Done()
			start := time.Now()
			err := chk.Probe(probeCtx)
			d := Detail{Status: "ok", Critical: chk.Critical, TookMs: float64(time.Since(start).Microseconds()) / 1000}
			if err != nil {
				d.Status = "fail"
				d.Error = err.Error()
			}
			results <- result{name: chk.Name, detail: d}
		}(chk)
	}
	wg.Wait()
	close(results)

	degraded, unavailable := false, false
	for res := range results {
		report.Checks[res.name] = res.detail
		if res.detail.Status == "fail" {
			if res.detail.Critical {
				unavailable = true
			} else {
				degraded = true
			}
		}
	}
	switch {
	case unavailable:
		report.Status = "unavailable"
	case degraded:
		report.Status = "degraded"
	}

	c.mu.Lock()
	snapshot := report
	c.lastResult = &snapshot
	c.lastChecked = time.Now()
	c.mu.Unlock()

	return report
}
