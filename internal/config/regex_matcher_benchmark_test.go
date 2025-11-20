package config

import (
	"regexp"
	"testing"

	"bot-detector/internal/types"
)

// Benchmark raw regex evaluation (baseline - no caching at all)
func BenchmarkRegexMatcher_RawRegex(b *testing.B) {
	re := regexp.MustCompile("^/login")
	path := "/login"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = re.MatchString(path)
	}
}

// Benchmark regex matcher WITHOUT result caching (only field caching)
// Simulates 10 chains checking same regex on same entry
func BenchmarkRegexMatcher_NoResultCache(b *testing.B) {
	re := regexp.MustCompile("^/login")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{Path: "/login"}

		// Simulate 10 chains evaluating same regex
		for chain := 0; chain < 10; chain++ {
			fieldVal := types.GetMatchValueIfType("Path", entry, types.StringField)
			if fieldVal != nil {
				_ = re.MatchString(fieldVal.(string))
			}
		}
	}
}

// Benchmark regex matcher WITH result caching
// Simulates 10 chains checking same regex on same entry
func BenchmarkRegexMatcher_WithResultCache(b *testing.B) {
	// Compile matcher with caching (simulates actual compiled matcher)
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, err := CompileStringMatcher(ctx, "regex:^/login")
	if err != nil {
		b.Fatalf("Failed to compile matcher: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{Path: "/login"}

		// Simulate 10 chains evaluating same regex matcher
		for chain := 0; chain < 10; chain++ {
			_ = matcher(entry)
		}
	}
}

// Benchmark realistic scenario: multiple different regexes on same entry
func BenchmarkRegexMatcher_MultiplePatterns(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	// Compile 3 different regex matchers
	matcher1, _ := CompileStringMatcher(ctx, "regex:^/login")
	matcher2, _ := CompileStringMatcher(ctx, "regex:^/api/")
	matcher3, _ := CompileStringMatcher(ctx, "regex:^/admin")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{Path: "/login"}

		// Each matcher evaluated multiple times (simulating multiple chains)
		for chain := 0; chain < 5; chain++ {
			_ = matcher1(entry)
			_ = matcher2(entry)
			_ = matcher3(entry)
		}
	}
}

// Benchmark complex regex pattern
func BenchmarkRegexMatcher_ComplexPattern(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	// Complex pattern that's expensive to evaluate
	matcher, _ := CompileStringMatcher(ctx, "regex:^/(api|admin|login)/(v[0-9]+)/([a-z]+)/([0-9]+)$")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{Path: "/api/v1/users/123"}

		// Simulate 10 chains checking this expensive regex
		for chain := 0; chain < 10; chain++ {
			_ = matcher(entry)
		}
	}
}

// Benchmark user's scenario: 10 chains, 2 matchers (path regex + statuscode list)
func BenchmarkRegexMatcher_UserScenario(b *testing.B) {
	pathCtx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	statusCtx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          1,
		CanonicalFieldName: "StatusCode",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	pathMatcher, _ := CompileStringMatcher(pathCtx, "regex:^/login")
	// Note: statuscode list matcher is not cached yet, so it will be slower
	// This benchmark shows the partial benefit of caching just regex

	statusValues := []interface{}{500, 501, 503}
	statusMatcher, _ := compileListMatcher(statusCtx, statusValues)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			Path:       "/login",
			StatusCode: 500,
		}

		// Simulate 10 chains, each checking path AND statuscode
		for chain := 0; chain < 10; chain++ {
			_ = pathMatcher(entry)
			_ = statusMatcher(entry)
		}
	}
}

// Benchmark cache hit rate measurement
func BenchmarkRegexMatcher_CacheHitRate(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, _ := CompileStringMatcher(ctx, "regex:^/login")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{Path: "/login"}

		// First call: cache miss
		_ = matcher(entry)

		// Next 99 calls: cache hits
		for j := 0; j < 99; j++ {
			_ = matcher(entry)
		}
	}
}
