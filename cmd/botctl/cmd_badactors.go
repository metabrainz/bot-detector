package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// badActor models one entry from GET /api/v1/bad-actors.
type badActor struct {
	IP          string  `json:"ip"`
	PromotedAt  string  `json:"promoted_at"`
	TotalScore  float64 `json:"total_score"`
	BlockCount  int     `json:"block_count"`
	HistoryJSON string  `json:"history,omitempty"`
}

// historyReasons parses the history JSON and returns the distinct reasons.
// A "null"/empty history yields no reasons.
func (b badActor) historyReasons() []string {
	if !hasEntries(b.HistoryJSON) {
		return nil
	}
	var entries []struct {
		Reason string `json:"r"`
	}
	if err := json.Unmarshal([]byte(b.HistoryJSON), &entries); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var reasons []string
	for _, e := range entries {
		if e.Reason != "" && !seen[e.Reason] {
			seen[e.Reason] = true
			reasons = append(reasons, e.Reason)
		}
	}
	return reasons
}

// matchesReason reports whether any history reason contains the substring.
func (b badActor) matchesReason(sub string) bool {
	for _, r := range b.historyReasons() {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// hasEntries reports whether a history JSON string carries at least one entry.
func hasEntries(historyJSON string) bool {
	if historyJSON == "" {
		return false
	}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(historyJSON), &entries); err != nil {
		return false
	}
	return len(entries) > 0
}

func cmdBadActorsList(ctx *cmdContext) int {
	reason, rest, err := extractStringFlag(ctx.args, "--reason")
	if err != nil {
		return usageErr(err.Error())
	}
	if len(rest) != 0 {
		return usageErr("usage: botctl bad-actors list [--reason <substr>]")
	}

	var actors []badActor
	body, err := ctx.c.doJSON("GET", "/api/v1/bad-actors", nil, &actors)
	if err != nil {
		return fail(err)
	}

	// Client-side reason filter mirrors the server's substring matching.
	if reason != "" {
		filtered := actors[:0:0]
		for _, a := range actors {
			if a.matchesReason(reason) {
				filtered = append(filtered, a)
			}
		}
		actors = filtered
	}

	if ctx.opts.json {
		if reason == "" {
			printRaw(body) // unfiltered: pass through verbatim
		} else {
			printJSON(actors)
		}
		return exitOK
	}

	if len(actors) == 0 {
		fmt.Println("No bad actors found.")
		return exitOK
	}
	for _, a := range actors {
		reasons := strings.Join(a.historyReasons(), ", ")
		if reasons == "" {
			reasons = "(no history)"
		}
		fmt.Printf("%-40s score=%-6.1f blocks=%-4d %s\n", a.IP, a.TotalScore, a.BlockCount, reasons)
	}
	fmt.Printf("\n%d bad actor(s).\n", len(actors))
	return exitOK
}

func cmdBadActorsStats(ctx *cmdContext) int {
	if len(ctx.args) != 0 {
		return usageErr("usage: botctl bad-actors stats")
	}
	var stats struct {
		Total         int            `json:"total"`
		AvgScore      float64        `json:"avg_score"`
		AvgBlockCount float64        `json:"avg_block_count"`
		ByReason      map[string]int `json:"by_reason"`
		ByDay         map[string]int `json:"by_day"`
	}
	body, err := ctx.c.doJSON("GET", "/api/v1/bad-actors/stats", nil, &stats)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}

	fmt.Printf("Total:           %d\n", stats.Total)
	fmt.Printf("Avg score:       %.1f\n", stats.AvgScore)
	fmt.Printf("Avg block count: %.1f\n", stats.AvgBlockCount)

	fmt.Println("\nIPs per reason:")
	for _, kv := range sortByCountDesc(stats.ByReason) {
		fmt.Printf("  %6d  %s\n", kv.value, kv.key)
	}

	fmt.Println("\nPromotions per day:")
	days := make([]string, 0, len(stats.ByDay))
	for d := range stats.ByDay {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		fmt.Printf("  %6d  %s\n", stats.ByDay[d], d)
	}
	return exitOK
}

func cmdBadActorsExport(ctx *cmdContext) int {
	if len(ctx.args) != 0 {
		return usageErr("usage: botctl bad-actors export")
	}
	body, err := ctx.c.do("GET", "/api/v1/bad-actors/export", nil)
	if err != nil {
		return fail(err)
	}
	printRaw(body)
	return exitOK
}

func cmdBadActorsRemove(ctx *cmdContext) int {
	reason, rest, err := extractStringFlag(ctx.args, "--reason")
	if err != nil {
		return usageErr(err.Error())
	}
	unblock, rest := extractBoolFlag(rest, "--unblock")
	if len(rest) != 0 {
		return usageErr("usage: botctl bad-actors remove --reason <substr> [--unblock]")
	}
	if reason == "" {
		return usageErr("bad-actors remove requires --reason <substr>")
	}

	action := "Remove"
	if unblock {
		action = "Remove and unblock"
	}
	if !confirm(ctx.opts, fmt.Sprintf("%s all bad actors matching reason %q?", action, reason)) {
		eprintln("aborted.")
		return exitOK
	}

	q := url.Values{}
	q.Set("reason", reason)
	if unblock {
		// The endpoint treats the presence of `unblock` (any value) as the flag.
		q.Set("unblock", "")
	}

	var res struct {
		Reason        string   `json:"reason"`
		Removed       int      `json:"removed"`
		IPs           []string `json:"ips"`
		Unblocked     int      `json:"unblocked,omitempty"`
		UnblockErrors []string `json:"unblock_errors,omitempty"`
	}
	body, err := ctx.c.doJSON("DELETE", "/api/v1/bad-actors", q, &res)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	fmt.Printf("Reason:    %s\n", res.Reason)
	fmt.Printf("Removed:   %d\n", res.Removed)
	if unblock {
		fmt.Printf("Unblocked: %d\n", res.Unblocked)
		if len(res.UnblockErrors) > 0 {
			fmt.Printf("Unblock errors: %s\n", strings.Join(res.UnblockErrors, ", "))
		}
	}
	return exitOK
}

// cmdBadActorsClear removes ALL bad actors (SQLite + HAProxy).
// Unblock is implied: clearing bad actors also unblocks them from HAProxy,
// unless --no-unblock is given (removes DB records only).
func cmdBadActorsClear(ctx *cmdContext) int {
	noUnblock, rest := extractBoolFlag(ctx.args, "--no-unblock")
	if len(rest) != 0 {
		return usageErr("usage: botctl bad-actors clear [--no-unblock]")
	}

	// Show how many will be affected before the destructive prompt.
	var actors []badActor
	if _, err := ctx.c.doJSON("GET", "/api/v1/bad-actors", nil, &actors); err != nil {
		return fail(err)
	}
	if len(actors) == 0 {
		fmt.Println("No bad actors to clear.")
		return exitOK
	}

	prompt := fmt.Sprintf("Clear ALL %d bad actor(s) and unblock them from HAProxy?", len(actors))
	if noUnblock {
		prompt = fmt.Sprintf("Clear ALL %d bad actor(s) from the database (leaving HAProxy blocks)?", len(actors))
	}
	if !confirm(ctx.opts, prompt) {
		eprintln("aborted.")
		return exitOK
	}

	q := url.Values{}
	q.Set("all", "")
	if !noUnblock {
		q.Set("unblock", "")
	}

	var res struct {
		All           bool     `json:"all"`
		Removed       int      `json:"removed"`
		IPs           []string `json:"ips"`
		Unblocked     int      `json:"unblocked,omitempty"`
		UnblockErrors []string `json:"unblock_errors,omitempty"`
	}
	body, err := ctx.c.doJSON("DELETE", "/api/v1/bad-actors", q, &res)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	fmt.Printf("Removed:   %d\n", res.Removed)
	if !noUnblock {
		fmt.Printf("Unblocked: %d\n", res.Unblocked)
		if len(res.UnblockErrors) > 0 {
			fmt.Printf("Unblock errors: %s\n", strings.Join(res.UnblockErrors, ", "))
		}
	}
	return exitOK
}

// kv is a key/count pair for sorted output.
type kv struct {
	key   string
	value int
}

// sortByCountDesc returns map entries sorted by count descending, then key.
func sortByCountDesc(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].value != out[j].value {
			return out[i].value > out[j].value
		}
		return out[i].key < out[j].key
	})
	return out
}
