// Command inventory-agent is GlueOps' read-only Kubernetes inventory
// collector. One run collects cluster/node versions, Helm releases and pod
// images, POSTs a single gzipped JSON snapshot, logs one summary line and
// exits 0 no matter what.
package main

import (
	"context"
	"fmt"
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
const runTimeout = 4 * time.Minute

func main() {
	os.Exit(run(os.LookupEnv))
}

func run(lookup func(string) (string, bool)) (code int) {
	level, _ := lookup("LOG_LEVEL")
	log := logging.New(level, os.Stdout)
	defer func() {
		if r := recover(); r != nil {
			log.Error("unrecovered panic", "reason", logging.ReasonInternalError, "panic", fmt.Sprint(r))
		}
		code = 0
	}()

	cfg, err := config.Load(lookup)
	if err != nil {
		log.Error("invalid configuration, nothing collected", "reason", logging.ReasonInvalidConfig, "error", err.Error())
		return 0
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
	return code
}
