package checker_test

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/checker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/testutil"
	"bot-detector/internal/utils"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baHarness is a test harness for bad actor integration tests.
type baHarness struct {
	t              *testing.T
	p              *app.Processor
	db             *sql.DB
	blockReasons   []string
	blockReasonsMu sync.Mutex
	logs           []string
	logsMu         sync.Mutex
}

func newBAHarness(t *testing.T, threshold float64) *baHarness {
	t.Helper()
	testutil.ResetGlobalState()

	db, err := persistence.OpenDB("", true)
	require.NoError(t, err)
	t.Cleanup(func() { persistence.CloseDB(db) })

	cfg := &config.AppConfig{
		BadActors: config.BadActorsConfig{
			Enabled:       true,
			Threshold:     threshold,
			BlockDuration: 168 * time.Hour,
		},
	}

	h := &baHarness{t: t, db: db}

	h.p = testutil.NewTestProcessor(cfg, nil)
	h.p.DB = db
	h.p.PersistenceEnabled = true
	h.p.Blocker = &baMockBlocker{
		blockFunc: func(ipInfo utils.IPInfo, d time.Duration, reason string) error {
			h.blockReasonsMu.Lock()
			h.blockReasons = append(h.blockReasons, reason)
			h.blockReasonsMu.Unlock()
			return nil
		},
		unblockFunc: func(utils.IPInfo, string) error { return nil },
	}
	h.p.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		msg := tag + ": " + fmt.Sprintf(format, args...)
		h.logsMu.Lock()
		h.logs = append(h.logs, msg)
		h.logsMu.Unlock()
	}

	return h
}

func (h *baHarness) addChain(name string, weight float64) {
	h.t.Helper()
	chain := config.BehavioralChain{
		Name:           name,
		Action:         "block",
		BlockDuration:  1 * time.Hour,
		MatchKey:       "ip",
		BadActorWeight: weight,
		StepsYAML: []config.StepDefYAML{
			{FieldMatches: map[string]interface{}{"path": "/"}},
		},
		MetricsCounter:      new(atomic.Int64),
		MetricsResetCounter: new(atomic.Int64),
		MetricsHitsCounter:  new(atomic.Int64),
	}

	// Compile matchers
	matchers, err := config.CompileMatchers(name, 0, chain.StepsYAML[0].FieldMatches, nil, "")
	require.NoError(h.t, err)
	chain.Steps = []config.StepDef{{Order: 1, Matchers: matchers}}
	h.p.Chains = append(h.p.Chains, chain)
}

func (h *baHarness) process(ip string) {
	h.t.Helper()
	entry := &app.LogEntry{
		IPInfo:    utils.NewIPInfo(ip),
		Timestamp: time.Now(),
		Path: "/",
	}
	checker.CheckChains(h.p, entry)
}

func (h *baHarness) clearActivity() {
	h.p.ActivityMutex.Lock()
	for k := range h.p.ActivityStore {
		delete(h.p.ActivityStore, k)
	}
	h.p.ActivityMutex.Unlock()
}

func (h *baHarness) hasBlockReason(reason string) bool {
	h.blockReasonsMu.Lock()
	defer h.blockReasonsMu.Unlock()
	for _, r := range h.blockReasons {
		if r == reason {
			return true
		}
	}
	return false
}

func (h *baHarness) hasLog(substr string) bool {
	h.logsMu.Lock()
	defer h.logsMu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// baMockBlocker implements blocker.Blocker for tests.
type baMockBlocker struct {
	blockFunc   func(utils.IPInfo, time.Duration, string) error
	unblockFunc func(utils.IPInfo, string) error
}

func (m *baMockBlocker) Block(ip utils.IPInfo, d time.Duration, r string) error { return m.blockFunc(ip, d, r) }
func (m *baMockBlocker) Unblock(ip utils.IPInfo, r string) error                { return m.unblockFunc(ip, r) }
func (m *baMockBlocker) Shutdown()                                               {}
func (m *baMockBlocker) IsIPBlocked(utils.IPInfo) (bool, error)                  { return false, nil }
func (m *baMockBlocker) DumpBackends() ([]string, error)                         { return nil, nil }
func (m *baMockBlocker) GetCurrentState() (map[string]int, error)                { return nil, nil }
func (m *baMockBlocker) ClearIP(utils.IPInfo) ([]interface{}, error)             { return nil, nil }
func (m *baMockBlocker) CompareHAProxyBackends(time.Duration) ([]blocker.SyncDiscrepancy, error) {
	return nil, nil
}

// --- Tests ---

func TestBadActor_ScoreAccumulation(t *testing.T) {
	h := newBAHarness(t, 5.0)
	h.addChain("chain1", 1.0)

	for i := 0; i < 3; i++ {
		h.clearActivity()
		h.process("10.0.0.1")
	}

	score, err := persistence.GetScore(h.db, "10.0.0.1")
	require.NoError(t, err)
	require.NotNil(t, score)
	assert.Equal(t, 3.0, score.Score)
	assert.Equal(t, 3, score.BlockCount)

	isBad, _ := persistence.IsBadActor(h.db, "10.0.0.1")
	assert.False(t, isBad, "should not be promoted at score 3.0 with threshold 5.0")
}

func TestBadActor_PromotionAtThreshold(t *testing.T) {
	h := newBAHarness(t, 3.0)
	h.addChain("chain1", 1.0)

	for i := 0; i < 3; i++ {
		h.clearActivity()
		h.process("10.0.0.2")
	}

	isBad, _ := persistence.IsBadActor(h.db, "10.0.0.2")
	assert.True(t, isBad, "should be promoted at score 3.0")

	assert.True(t, h.hasBlockReason("bad-actor"), "should issue bad-actor block")

	ba, _ := persistence.GetBadActor(h.db, "10.0.0.2")
	require.NotNil(t, ba)
	assert.Equal(t, 3.0, ba.TotalScore)
	assert.NotEmpty(t, ba.HistoryJSON)
}

func TestBadActor_WeightedScoring(t *testing.T) {
	h := newBAHarness(t, 5.0)
	h.addChain("high", 1.0)
	h.addChain("low", 0.3)

	// Both chains match, but second chain won't fire because IP is blocked after first.
	// So each process() adds only 1.0 from the first chain.
	for i := 0; i < 2; i++ {
		h.clearActivity()
		h.process("10.0.0.3")
	}

	score, _ := persistence.GetScore(h.db, "10.0.0.3")
	require.NotNil(t, score)
	assert.InDelta(t, 2.0, score.Score, 0.01, "2 entries × 1.0 (second chain skipped due to block)")

	isBad, _ := persistence.IsBadActor(h.db, "10.0.0.3")
	assert.False(t, isBad)
}

func TestBadActor_SkipsChainProcessing(t *testing.T) {
	h := newBAHarness(t, 1.0)
	h.addChain("chain1", 1.0)

	// First entry → block + promotion
	h.process("10.0.0.4")
	isBad, _ := persistence.IsBadActor(h.db, "10.0.0.4")
	assert.True(t, isBad)

	// Clear tracking
	h.blockReasonsMu.Lock()
	h.blockReasons = nil
	h.blockReasonsMu.Unlock()
	h.clearActivity()

	// Second entry — should be caught by bad actor check, no chain processing
	h.process("10.0.0.4")

	assert.False(t, h.hasBlockReason("chain1"), "chain should not fire for bad actor")
}

func TestBadActor_GoodActorTakesPriority(t *testing.T) {
	h := newBAHarness(t, 1.0)
	h.addChain("chain1", 1.0)

	// Configure good actor
	h.p.ConfigMutex.Lock()
	h.p.Config.GoodActors = []config.GoodActorDef{
		{
			Name: "trusted",
			IPMatchers: []config.FieldMatcher{
				func(entry *app.LogEntry) bool { return entry.IPInfo.Address == "10.0.0.5" },
			},
		},
	}
	h.p.ConfigMutex.Unlock()

	// Manually promote to bad actor
	require.NoError(t, persistence.PromoteToBadActor(h.db, "10.0.0.5", 5.0, 5, time.Now()))

	h.blockReasonsMu.Lock()
	h.blockReasons = nil
	h.blockReasonsMu.Unlock()

	h.process("10.0.0.5")

	// Good actor should prevent any blocking
	assert.False(t, h.hasBlockReason("chain1"), "good actor should prevent chain blocking")
	assert.False(t, h.hasBlockReason("bad-actor"), "good actor should prevent bad actor blocking")
}

func TestBadActor_Removal(t *testing.T) {
	db, err := persistence.OpenDB("", true)
	require.NoError(t, err)
	defer persistence.CloseDB(db)

	now := time.Now()
	_, _, _ = persistence.IncrementScore(db, "10.0.0.6", 5.0, now)
	require.NoError(t, persistence.PromoteToBadActor(db, "10.0.0.6", 5.0, 5, now))
	require.NoError(t, persistence.UpsertIPState(db, "10.0.0.6", persistence.BlockStateBlocked, now.Add(168*time.Hour), "bad-actor", now, now))

	// Remove
	require.NoError(t, persistence.RemoveBadActor(db, "10.0.0.6"))
	require.NoError(t, persistence.DeleteIPState(db, "10.0.0.6"))

	isBad, _ := persistence.IsBadActor(db, "10.0.0.6")
	assert.False(t, isBad)
	score, _ := persistence.GetScore(db, "10.0.0.6")
	assert.Nil(t, score)
	state, _ := persistence.GetIPState(db, "10.0.0.6")
	assert.Nil(t, state)
}

func TestBadActor_DisabledConfig(t *testing.T) {
	testutil.ResetGlobalState()
	db, err := persistence.OpenDB("", true)
	require.NoError(t, err)
	defer persistence.CloseDB(db)

	p := testutil.NewTestProcessor(&config.AppConfig{
		BadActors: config.BadActorsConfig{Enabled: false},
	}, nil)
	p.DB = db
	p.PersistenceEnabled = true
	p.Blocker = &baMockBlocker{
		blockFunc:   func(utils.IPInfo, time.Duration, string) error { return nil },
		unblockFunc: func(utils.IPInfo, string) error { return nil },
	}

	chain := config.BehavioralChain{
		Name: "chain1", Action: "block", BlockDuration: 1 * time.Hour,
		MatchKey: "ip", BadActorWeight: 1.0,
		StepsYAML:           []config.StepDefYAML{{FieldMatches: map[string]interface{}{"path": "/"}}},
		MetricsCounter:      new(atomic.Int64),
		MetricsResetCounter: new(atomic.Int64),
		MetricsHitsCounter:  new(atomic.Int64),
	}
	matchers, _ := config.CompileMatchers("chain1", 0, chain.StepsYAML[0].FieldMatches, nil, "")
	chain.Steps = []config.StepDef{{Order: 1, Matchers: matchers}}
	p.Chains = []config.BehavioralChain{chain}

	entry := &app.LogEntry{IPInfo: utils.NewIPInfo("10.0.0.7"), Timestamp: time.Now(), Path: "/"}
	checker.CheckChains(p, entry)

	score, _ := persistence.GetScore(db, "10.0.0.7")
	assert.Nil(t, score, "no scoring when bad_actors disabled")
}

func TestBadActor_ZeroWeight(t *testing.T) {
	h := newBAHarness(t, 5.0)
	h.addChain("zero-weight", 0.0)

	h.process("10.0.0.8")

	score, _ := persistence.GetScore(h.db, "10.0.0.8")
	assert.Nil(t, score, "zero-weight chain should not score")
}

func TestBadActor_NoDuplicatePromotion(t *testing.T) {
	h := newBAHarness(t, 1.0)
	h.addChain("chain1", 1.0)

	h.process("10.0.0.9")
	assert.True(t, h.hasLog("promoted to bad actor"))

	// Clear and re-process
	h.clearActivity()
	h.logsMu.Lock()
	h.logs = nil
	h.logsMu.Unlock()

	h.process("10.0.0.9")
	assert.False(t, h.hasLog("promoted to bad actor"), "should not promote twice")
}

func TestBadActor_PersistenceDisabled_NoPanic(t *testing.T) {
	testutil.ResetGlobalState()
	p := testutil.NewTestProcessor(&config.AppConfig{
		BadActors: config.BadActorsConfig{Enabled: true, Threshold: 5.0, BlockDuration: 168 * time.Hour},
	}, nil)
	p.PersistenceEnabled = false
	p.Blocker = &baMockBlocker{
		blockFunc:   func(utils.IPInfo, time.Duration, string) error { return nil },
		unblockFunc: func(utils.IPInfo, string) error { return nil },
	}

	chain := config.BehavioralChain{
		Name: "chain1", Action: "block", BlockDuration: 1 * time.Hour,
		MatchKey: "ip", BadActorWeight: 1.0,
		StepsYAML:           []config.StepDefYAML{{FieldMatches: map[string]interface{}{"path": "/"}}},
		MetricsCounter:      new(atomic.Int64),
		MetricsResetCounter: new(atomic.Int64),
		MetricsHitsCounter:  new(atomic.Int64),
	}
	matchers, _ := config.CompileMatchers("chain1", 0, chain.StepsYAML[0].FieldMatches, nil, "")
	chain.Steps = []config.StepDef{{Order: 1, Matchers: matchers}}
	p.Chains = []config.BehavioralChain{chain}

	entry := &app.LogEntry{IPInfo: utils.NewIPInfo("10.0.0.10"), Timestamp: time.Now(), Path: "/"}
	assert.NotPanics(t, func() { checker.CheckChains(p, entry) })
}
