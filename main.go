// Command inventory-agent is GlueOps' read-only Kubernetes inventory
// collector. One run collects cluster/node versions, Helm releases and pod
// images, POSTs a single gzipped JSON snapshot, logs one summary line and
// exits 0 no matter what.
package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"time"

	"github.com/glueops/inventory-agent/internal/agent"
	"github.com/glueops/inventory-agent/internal/config"
	"github.com/glueops/inventory-agent/internal/kube"
	"github.com/glueops/inventory-agent/internal/logging"
)

// version is set at build time: -ldflags="-X main.version=v1.2.3".
var version = "dev"

// runTimeout is a far-off backstop below the CronJob's activeDeadlineSeconds.
// It starts after the START_JITTER delay, so the chart's deadline must cover
// both: START_JITTER + runTimeout + slack.
const runTimeout = 4 * time.Minute

func main() {
	os.Exit(run(os.LookupEnv, os.Stdout, time.Sleep))
}

// run is the whole process: it always returns 0 and always ends with exactly
// one "run summary" log line, whichever path the run takes. sleep is
// time.Sleep outside tests.
func run(lookup func(string) (string, bool), out io.Writer, sleep func(time.Duration)) (code int) {
	level, _ := lookup("LOG_LEVEL")
	log := logging.New(level, out)
	summarized := false
	defer func() {
		if r := recover(); r != nil {
			log.Error("unrecovered panic", "reason", logging.ReasonInternalError, "panic", fmt.Sprint(r))
			if !summarized {
				agent.LogSummary(log, agent.Summary{
					RunID:  agent.NewRunID(time.Now()),
					Status: agent.StatusInternalError,
					Reason: logging.ReasonInternalError,
					Error:  "panic: " + fmt.Sprint(r),
				})
			}
		}
		code = 0
	}()

	cfg, err := config.Load(lookup)
	if err != nil {
		log.Error("invalid configuration, nothing collected", "reason", logging.ReasonInvalidConfig, "error", err.Error())
		agent.LogSummary(log, agent.Summary{
			RunID:  agent.NewRunID(time.Now()),
			Status: agent.StatusInvalidConfig,
			Reason: logging.ReasonInvalidConfig,
			Error:  err.Error(),
		})
		summarized = true
		return 0
	}

	// Spread the fleet: every captain runs on the same schedule, so without a
	// delay they all POST to the receiver at the same instant. The delay comes
	// before the run timeout and the run ID, so it neither eats the run's
	// budget nor makes collected_at earlier than the data. Nothing to send,
	// nothing to spread.
	if cfg.StartJitter > 0 && cfg.IngestURL != "" {
		delay := rand.N(cfg.StartJitter)
		log.Info("start delayed", "delay_ms", delay.Milliseconds(), "max_ms", cfg.StartJitter.Milliseconds())
		sleep(delay)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	deps := agent.Deps{Version: version}
	client, err := kube.NewClient("inventory-agent/"+version, 30*time.Second)
	if err != nil {
		log.Error("kubernetes client unavailable; every section will report api_unavailable",
			"reason", logging.ReasonKubeClientUnavailable, "error", err.Error())
	} else {
		deps.Client = client
	}

	code, _ = agent.Run(ctx, cfg, deps, log)
	summarized = true
	return code
}
