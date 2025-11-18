package config

import (
	"bot-detector/internal/persistence"
	"bot-detector/internal/types"
	"io"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// FileOpener defines the function signature for opening a file.
type FileOpener func(name string) (FileHandle, error)

// FileHandle defines the interface for file operations needed by the tailer.
type FileHandle interface {
	io.ReadSeeker
	io.Closer
	Stat() (os.FileInfo, error)
}

// Config type definitions extracted from internal/app/types.go
type AppConfig struct {
	Application      ApplicationConfig                 `config:"compare"`
	Parser           ParserConfig                      `config:"compare"`
	Checker          CheckerConfig                     `config:"compare"`
	Blockers         BlockersConfig                    `config:"compare"`
	GoodActors       []GoodActorDef                    `config:"compare"`
	FileDependencies map[string]*types.FileDependency  // Map of file paths to their dependency status.
	LastModTime      time.Time                         // Not compared
	StatFunc         func(string) (os.FileInfo, error) // Mockable
	FileOpener       FileOpener                        // Mockable
	YAMLContent      []byte
}

type ApplicationConfig struct {
	LogLevel        string                        `config:"compare"`
	EnableMetrics   bool                          `config:"compare" summary:"enable_metrics"`
	Config          ConfigManagement              `config:"compare"`
	Persistence     persistence.PersistenceConfig `config:"compare"`
	EOFPollingDelay time.Duration                 `config:"compare" summary:"eof_polling_delay"`
}

type ConfigManagement struct {
	PollingInterval time.Duration `config:"compare" summary:"polling_interval"`
}

type ParserConfig struct {
	LineEnding          string        `config:"compare" summary:"line_ending"`
	OutOfOrderTolerance time.Duration `config:"compare" summary:"out_of_order_tolerance"`
	TimestampFormat     string        `config:"compare"`
	LogFormatRegex      string        `config:"compare"`
}

type CheckerConfig struct {
	UnblockOnGoodActor    bool          `config:"compare"`
	UnblockCooldown       time.Duration `config:"compare"`
	ActorCleanupInterval  time.Duration `config:"compare" summary:"cleanup_interval"`
	ActorStateIdleTimeout time.Duration `config:"compare" summary:"idle_timeout"`
	MaxTimeSinceLastHit   time.Duration `config:"compare" summary:"max_time_since_last_hit"`
}

type BlockersConfig struct {
	DefaultDuration   time.Duration `config:"compare" summary:"default_block_duration"`
	CommandsPerSecond int           `config:"compare" summary:"blocker_commands_per_second"`
	CommandQueueSize  int           `config:"compare" summary:"blocker_command_queue_size"`
	DialTimeout       time.Duration `config:"compare" summary:"blocker_dial_timeout"`
	MaxRetries        int           `config:"compare" summary:"blocker_max_retries"`
	RetryDelay        time.Duration `config:"compare" summary:"blocker_retry_delay"`
	Backends          Backends      `config:"compare"`
}

type Backends struct {
	HAProxy HAProxyConfig `config:"compare"`
}

type HAProxyConfig struct {
	Addresses         []string                 `config:"compare" summary:"blocker_addresses"`
	DurationTables    map[time.Duration]string `config:"compare" summary:"duration_tables"`
	TableNameFallback string                   `config:"compare"`
}

type LoadedConfig struct {
	Application      ApplicationConfig
	Parser           ParserConfig
	Checker          CheckerConfig
	Blockers         BlockersConfig
	GoodActors       []GoodActorDef    `config:"compare"`
	Chains           []BehavioralChain // Not compared here
	FileDependencies map[string]*types.FileDependency
	LogFormatRegex   *regexp.Regexp // Not compared here
	StatFunc         func(string) (os.FileInfo, error)
	YAMLContent      []byte
}

type ChainConfig struct {
	Version     string                   `yaml:"version"`
	Application ApplicationConfigYAML    `yaml:"application"`
	Parser      ParserConfigYAML         `yaml:"parser"`
	Checker     CheckerConfigYAML        `yaml:"checker"`
	Blockers    BlockersConfigYAML       `yaml:"blockers"`
	GoodActors  []map[string]interface{} `yaml:"good_actors"`
	Chains      []BehavioralChainYAML    `yaml:"chains"`
}

type ApplicationConfigYAML struct {
	LogLevel        string                        `yaml:"log_level"`
	EnableMetrics   *bool                         `yaml:"enable_metrics"`
	Config          ConfigManagementYAML          `yaml:"config"`
	Persistence     persistence.PersistenceConfig `yaml:"persistence"`
	EOFPollingDelay string                        `yaml:"eof_polling_delay"`
}

type ConfigManagementYAML struct {
	PollingInterval string `yaml:"polling_interval"`
}

type ParserConfigYAML struct {
	LineEnding          string `yaml:"line_ending"`
	OutOfOrderTolerance string `yaml:"out_of_order_tolerance"`
	TimestampFormat     string `yaml:"timestamp_format"`
	LogFormatRegex      string `yaml:"log_format_regex"`
}

type CheckerConfigYAML struct {
	UnblockOnGoodActor    bool   `yaml:"unblock_on_good_actor"`
	UnblockCooldown       string `yaml:"unblock_cooldown"`
	ActorCleanupInterval  string `yaml:"actor_cleanup_interval"`
	ActorStateIdleTimeout string `yaml:"actor_state_idle_timeout"`
}

type BlockersConfigYAML struct {
	DefaultDuration   string       `yaml:"default_duration"`
	CommandsPerSecond int          `yaml:"commands_per_second"`
	CommandQueueSize  int          `yaml:"command_queue_size"`
	DialTimeout       string       `yaml:"dial_timeout"`
	MaxRetries        int          `yaml:"max_retries"`
	RetryDelay        string       `yaml:"retry_delay"`
	Backends          BackendsYAML `yaml:"backends"`
}

type BackendsYAML struct {
	HAProxy HAProxyConfigYAML `yaml:"haproxy"`
}

type HAProxyConfigYAML struct {
	Addresses      []string          `yaml:"addresses"`
	DurationTables map[string]string `yaml:"duration_tables"`
}
type StepDefYAML struct {
	Order               int
	FieldMatches        map[string]interface{} `yaml:"field_matches"`
	MaxDelay            string                 `yaml:"max_delay"`
	MinDelay            string                 `yaml:"min_delay"`
	MinTimeSinceLastHit string                 `yaml:"min_time_since_last_hit"`
	Repeated            int                    `yaml:"repeated"`
}

type BehavioralChainYAML struct {
	Name          string        `yaml:"name"`
	Action        string        `yaml:"action"`
	BlockDuration string        `yaml:"block_duration"`
	MatchKey      string        `yaml:"match_key"`
	OnMatch       string        `yaml:"on_match"`
	Steps         []StepDefYAML `yaml:"steps"`
}

// --- RUNTIME DATA STRUCTURES ---
type StepDef struct {
	Order    int
	Matchers []struct {
		Matcher   fieldMatcher
		FieldName string
	} // Changed: Now stores matcher and its associated field name.
	MaxDelayDuration    time.Duration
	MinDelayDuration    time.Duration
	MinTimeSinceLastHit time.Duration
}

// BehavioralChain holds the compiled definition of a single behavioral chain.
type BehavioralChain struct {
	Name                     string
	Action                   string
	BlockDuration            time.Duration
	BlockDurationStr         string               // The original string representation of the duration (e.g., "1w")
	UsesDefaultBlockDuration bool                 // True if the chain is using the global default_block_duration.
	MatchKey                 string               // (ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua)
	OnMatch                  string               // "stop" to halt processing of other chains on match.
	StepsYAML                []StepDefYAML // Store original YAML for accurate comparison
	Steps                    []StepDef
	MetricsHitsCounter       *atomic.Int64 // Counter for hits on this specific chain.
	MetricsResetCounter      *atomic.Int64 // Counter for resets of this specific chain.
	MetricsCounter           *atomic.Int64 // Counter for this specific chain.
	FieldMatchCounts         *sync.Map     // Counter for field matches within this chain (key: fieldName, value: *atomic.Int64).
}

// GoodActorDef represents a single compiled definition from the good_actors config.

type GoodActorDef struct {
	Name string

	IPMatchers []fieldMatcher // A list of matchers for the IP field (OR logic within the list)

	UAMatchers []fieldMatcher // A list of matchers for the UserAgent field (OR logic within the list)

}

// Clone creates a deep copy of the AppConfig object.
func (ac *AppConfig) Clone() AppConfig {
	if ac == nil {
		return AppConfig{}
	}

	// Clone FileDependencies map
	fileDeps := make(map[string]*types.FileDependency, len(ac.FileDependencies))
	for path, dep := range ac.FileDependencies {
		fileDeps[path] = dep.Clone()
	}

	// Clone GoodActors slice
	goodActors := make([]GoodActorDef, len(ac.GoodActors))
	copy(goodActors, ac.GoodActors)

	// Clone YAMLContent
	yamlCopy := make([]byte, len(ac.YAMLContent))
	copy(yamlCopy, ac.YAMLContent)

	return AppConfig{
		Application:      ac.Application,
		Parser:           ac.Parser,
		Checker:          ac.Checker,
		Blockers:         ac.Blockers,
		GoodActors:       goodActors,
		FileDependencies: fileDeps,
		LastModTime:      ac.LastModTime,
		StatFunc:         ac.StatFunc,
		FileOpener:       ac.FileOpener,
		YAMLContent:      yamlCopy,
	}
}
