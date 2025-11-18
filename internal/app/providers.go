package app

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- MetricsProvider Interface Implementation ---

// GetConfigForArchive safely retrieves the main config content and its dependencies for archiving.
func (p *Processor) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()

	// Create a deep copy of the dependencies to avoid race conditions if the config is reloaded
	// while the archive is being generated in a goroutine.
	depsCopy := make(map[string]*types.FileDependency)
	for path, dep := range p.Config.FileDependencies {
		// We only include files that are currently loaded and exist.
		if dep.CurrentStatus != nil && dep.CurrentStatus.Status == types.FileStatusLoaded {
			depsCopy[path] = dep.Clone()
		}
	}

	return p.Config.YAMLContent, p.Config.LastModTime, depsCopy, p.ConfigDir, nil
}

// GetListenAddr returns the HTTP listen address from the config.
func (p *Processor) GetListenAddr() string {
	return p.HTTPServer
}

// GetShutdownChannel returns the channel used for shutdown signals.
func (p *Processor) GetShutdownChannel() chan os.Signal {
	return p.SignalCh
}

// Log is a wrapper around the processor's LogFunc to satisfy the interface.
func (p *Processor) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	p.LogFunc(level, tag, format, v...)
}

// --- StoreProvider Interface Implementation ---

func (p *Processor) GetCleanupInterval() time.Duration {
	return p.Config.Checker.ActorCleanupInterval
}

func (p *Processor) GetIdleTimeout() time.Duration {
	return p.Config.Checker.ActorStateIdleTimeout
}

func (p *Processor) GetMaxTimeSinceLastHit() time.Duration {
	return p.Config.Checker.MaxTimeSinceLastHit
}

func (p *Processor) GetTopN() int {
	return p.TopN
}

func (p *Processor) GetTopActorsPerChain() map[string]map[string]*store.ActorStats {
	return p.TopActorsPerChain
}

func (p *Processor) GetActivityStore() map[store.Actor]*store.ActorActivity {
	return p.ActivityStore
}

func (p *Processor) GetActivityMutex() *sync.RWMutex {
	return p.ActivityMutex
}

func (p *Processor) GetTestSignals() *store.TestSignals {
	if p.TestSignals == nil {
		return nil
	}
	// Convert main.TestSignals to store.TestSignals
	return &store.TestSignals{
		CleanupDoneSignal: p.TestSignals.CleanupDoneSignal,
	}
}

func (p *Processor) IncrementActorsCleaned() {
	p.Metrics.ActorsCleaned.Add(1)
}

// --- MetricsProvider Interface Implementation ---

func (p *Processor) IncrementBlockerCmdsQueued() {
	p.Metrics.BlockerCmdsQueued.Add(1)
}

func (p *Processor) IncrementBlockerCmdsDropped() {
	p.Metrics.BlockerCmdsDropped.Add(1)
}

func (p *Processor) IncrementBlockerCmdsExecuted() {
	p.Metrics.BlockerCmdsExecuted.Add(1)
}

// --- HAProxyProvider Interface Implementation ---

func (p *Processor) GetBlockerAddresses() []string {
	return p.Config.Blockers.Backends.HAProxy.Addresses
}

func (p *Processor) GetDurationTables() map[time.Duration]string {
	return p.Config.Blockers.Backends.HAProxy.DurationTables
}

func (p *Processor) GetBlockTableNameFallback() string {
	return p.Config.Blockers.Backends.HAProxy.TableNameFallback
}

func (p *Processor) GetBlockerMaxRetries() int {
	return p.Config.Blockers.MaxRetries
}

func (p *Processor) GetBlockerRetryDelay() time.Duration {
	return p.Config.Blockers.RetryDelay
}

func (p *Processor) GetBlockerDialTimeout() time.Duration {
	return p.Config.Blockers.DialTimeout
}

func (p *Processor) IncrementBlockerRetries() {
	p.Metrics.BlockerRetries.Add(1)
}

func (p *Processor) IncrementCmdsPerBlocker(addr string) {
	if val, ok := p.Metrics.CmdsPerBlocker.Load(addr); ok {
		val.(*atomic.Int64).Add(1)
	}
}

// GenerateHTMLMetricsReport creates the full metrics report as an HTML-safe string.
func (p *Processor) GenerateHTMLMetricsReport() string {
	var report strings.Builder
	webLogFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Sanitize the formatted string before writing it to the HTML report.
		report.WriteString(utils.ForHTML(fmt.Sprintf(format, args...)) + "\n")
	}
	LogMetricsSummary(p, time.Since(p.StartTime), webLogFunc, "METRICS", "metric")
	return report.String()
}

// GenerateStepsMetricsReport creates a report of step execution counts as an HTML-safe string.
func (p *Processor) GenerateStepsMetricsReport() string {
	var report strings.Builder
	report.WriteString("--- Step Execution Counts ---\n")
	if p.Metrics.StepExecutionCounts == nil {
		report.WriteString("Step metrics are not enabled or initialized.\n")
		return report.String()
	}

	// Collect and sort step metrics for consistent output
	type StepMetric struct {
		Name  string
		Count int64
	}
	var stepMetrics []StepMetric
	p.Metrics.StepExecutionCounts.Range(func(key, value interface{}) bool {
		stepName, _ := key.(string)
		count, _ := value.(*atomic.Int64)
		stepMetrics = append(stepMetrics, StepMetric{Name: stepName, Count: count.Load()})
		return true
	})

	sort.Slice(stepMetrics, func(i, j int) bool {
		if stepMetrics[i].Count == stepMetrics[j].Count {
			return stepMetrics[i].Name < stepMetrics[j].Name
		}
		return stepMetrics[i].Count >= stepMetrics[j].Count
	})

	for _, sm := range stepMetrics {
		report.WriteString(fmt.Sprintf("%12d %s\n", sm.Count, utils.ForHTML(sm.Name)))
	}
	return report.String()
}
