package config

import (
	"net"
	"testing"

	"bot-detector/internal/types"
	"bot-detector/internal/utils"
)

// Benchmark raw CIDR evaluation (baseline - no caching at all)
func BenchmarkCIDRMatcher_RawCIDR(b *testing.B) {
	_, ipNet, _ := net.ParseCIDR("192.168.0.0/16")
	ip := net.ParseIP("192.168.1.100")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ipNet.Contains(ip)
	}
}

// Benchmark CIDR matcher with net.ParseIP overhead
func BenchmarkCIDRMatcher_WithParsing(b *testing.B) {
	_, ipNet, _ := net.ParseCIDR("192.168.0.0/16")
	ipStr := "192.168.1.100"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ip := net.ParseIP(ipStr)
		_ = ipNet.Contains(ip)
	}
}

// Benchmark CIDR matcher WITHOUT result caching (only field caching)
// Simulates 10 chains checking same CIDR on same entry
func BenchmarkCIDRMatcher_NoResultCache(b *testing.B) {
	_, ipNet, _ := net.ParseCIDR("192.168.0.0/16")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("192.168.1.100"),
		}

		// Simulate 10 chains evaluating same CIDR
		for chain := 0; chain < 10; chain++ {
			fieldVal := types.GetMatchValueIfType("IP", entry, types.StringField)
			if fieldVal != nil {
				ip := net.ParseIP(fieldVal.(string))
				if ip != nil {
					_ = ipNet.Contains(ip)
				}
			}
		}
	}
}

// Benchmark CIDR matcher WITH result caching
// Simulates 10 chains checking same CIDR on same entry
func BenchmarkCIDRMatcher_WithResultCache(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, err := CompileStringMatcher(ctx, "cidr:192.168.0.0/16")
	if err != nil {
		b.Fatalf("Failed to compile matcher: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("192.168.1.100"),
		}

		// Simulate 10 chains evaluating same CIDR matcher
		for chain := 0; chain < 10; chain++ {
			_ = matcher(entry)
		}
	}
}

// Benchmark multiple different CIDR matchers on same entry
func BenchmarkCIDRMatcher_MultiplePatterns(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	// Compile 3 different CIDR matchers
	matcher1, _ := CompileStringMatcher(ctx, "cidr:192.168.0.0/16")
	matcher2, _ := CompileStringMatcher(ctx, "cidr:10.0.0.0/8")
	matcher3, _ := CompileStringMatcher(ctx, "cidr:172.16.0.0/12")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("192.168.1.100"),
		}

		// Each matcher evaluated multiple times (simulating multiple chains)
		for chain := 0; chain < 5; chain++ {
			_ = matcher1(entry)
			_ = matcher2(entry)
			_ = matcher3(entry)
		}
	}
}

// Benchmark IPv6 CIDR matcher
func BenchmarkCIDRMatcher_IPv6(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, _ := CompileStringMatcher(ctx, "cidr:2001:db8::/32")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("2001:db8::1"),
		}

		// Simulate 10 chains checking same IPv6 CIDR
		for chain := 0; chain < 10; chain++ {
			_ = matcher(entry)
		}
	}
}

// Benchmark realistic scenario: 10 chains, mix of regex and CIDR
func BenchmarkCIDRMatcher_MixedWithRegex(b *testing.B) {
	pathCtx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "Path",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	ipCtx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          1,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	pathMatcher, _ := CompileStringMatcher(pathCtx, "regex:^/api/")
	cidrMatcher, _ := CompileStringMatcher(ipCtx, "cidr:192.168.0.0/16")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			Path:   "/api/test",
			IPInfo: utils.NewIPInfo("192.168.1.100"),
		}

		// Simulate 10 chains, each checking path AND IP
		for chain := 0; chain < 10; chain++ {
			_ = pathMatcher(entry)
			_ = cidrMatcher(entry)
		}
	}
}

// Benchmark cache hit rate for CIDR
func BenchmarkCIDRMatcher_CacheHitRate(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, _ := CompileStringMatcher(ctx, "cidr:192.168.0.0/16")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("192.168.1.100"),
		}

		// First call: cache miss
		_ = matcher(entry)

		// Next 99 calls: cache hits
		for j := 0; j < 99; j++ {
			_ = matcher(entry)
		}
	}
}

// Benchmark narrow vs wide CIDR range
func BenchmarkCIDRMatcher_NarrowRange(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, _ := CompileStringMatcher(ctx, "cidr:192.168.1.0/24")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("192.168.1.100"),
		}

		for chain := 0; chain < 10; chain++ {
			_ = matcher(entry)
		}
	}
}

func BenchmarkCIDRMatcher_WideRange(b *testing.B) {
	ctx := MatcherContext{
		ChainName:          "TestChain",
		StepIndex:          0,
		CanonicalFieldName: "IP",
		FileDependencies:   make(map[string]*types.FileDependency),
		FilePath:           "/test/config.yaml",
	}

	matcher, _ := CompileStringMatcher(ctx, "cidr:10.0.0.0/8")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry := &types.LogEntry{
			IPInfo: utils.NewIPInfo("10.123.45.67"),
		}

		for chain := 0; chain < 10; chain++ {
			_ = matcher(entry)
		}
	}
}
