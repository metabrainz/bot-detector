package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func cmdBlocksUnblock(ctx *cmdContext) int {
	reason, rest, err := extractStringFlag(ctx.args, "--reason")
	if err != nil {
		return usageErr(err.Error())
	}
	if len(rest) != 0 {
		return usageErr("usage: botctl blocks unblock --reason <substr>")
	}
	if reason == "" {
		return usageErr("blocks unblock requires --reason <substr>")
	}

	if !confirm(ctx.opts, fmt.Sprintf("Unblock all IPs blocked by reason %q?", reason)) {
		eprintln("aborted.")
		return exitOK
	}

	q := url.Values{}
	q.Set("reason", reason)

	var res struct {
		Reason    string   `json:"reason"`
		Matched   int      `json:"matched"`
		Unblocked int      `json:"unblocked"`
		Errors    []string `json:"errors,omitempty"`
	}
	body, err := ctx.c.doJSON("POST", "/api/v1/blocks/unblock", q, &res)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	fmt.Printf("Reason:    %s\n", res.Reason)
	fmt.Printf("Matched:   %d\n", res.Matched)
	fmt.Printf("Unblocked: %d\n", res.Unblocked)
	if len(res.Errors) > 0 {
		fmt.Printf("Errors:    %s\n", strings.Join(res.Errors, ", "))
	}
	return exitOK
}

func cmdConfigShow(ctx *cmdContext) int {
	if len(ctx.args) != 0 {
		return usageErr("usage: botctl config show")
	}
	body, err := ctx.c.do("GET", "/config", nil)
	if err != nil {
		return fail(err)
	}
	printRaw(body)
	return exitOK
}

func cmdConfigArchive(ctx *cmdContext) int {
	out, rest, err := extractStringFlag(ctx.args, "-o")
	if err != nil {
		return usageErr(err.Error())
	}
	if len(rest) != 0 {
		return usageErr("usage: botctl config archive [-o <file>]")
	}
	if out == "" {
		out = "bot-detector-config.tar.gz"
	}

	body, err := ctx.c.do("GET", "/config/archive", nil)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(out, body, 0o644); err != nil {
		return fail(fmt.Errorf("write %s: %w", out, err))
	}
	fmt.Printf("Wrote %s (%d bytes).\n", out, len(body))
	return exitOK
}

func cmdMetricsShow(ctx *cmdContext) int {
	aggregate, rest := extractBoolFlag(ctx.args, "--aggregate")
	if len(rest) != 0 {
		return usageErr("usage: botctl metrics show [--aggregate]")
	}

	path := "/api/v1/cluster/metrics"
	if aggregate {
		path = "/api/v1/cluster/metrics/aggregate"
	}
	body, err := ctx.c.do("GET", path, nil)
	if err != nil {
		return fail(err)
	}
	// Metrics are JSON; print raw unless we can pretty-print.
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	var generic any
	if json.Unmarshal(body, &generic) == nil {
		printJSON(generic)
	} else {
		printRaw(body)
	}
	return exitOK
}

func cmdClusterStatus(ctx *cmdContext) int {
	if len(ctx.args) != 0 {
		return usageErr("usage: botctl cluster status")
	}
	var st struct {
		Role    string `json:"role"`
		Name    string `json:"name"`
		Address string `json:"address"`
	}
	body, err := ctx.c.doJSON("GET", "/api/v1/cluster/status", nil, &st)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	fmt.Printf("Role:    %s\n", st.Role)
	fmt.Printf("Name:    %s\n", st.Name)
	fmt.Printf("Address: %s\n", st.Address)
	return exitOK
}

func cmdClusterState(ctx *cmdContext) int {
	reason, rest, err := extractStringFlag(ctx.args, "--reason")
	if err != nil {
		return usageErr(err.Error())
	}
	if len(rest) != 0 {
		return usageErr("usage: botctl cluster state [--reason <substr>]")
	}

	body, err := ctx.c.do("GET", "/api/v1/cluster/state/merged", nil)
	if err != nil {
		return fail(err)
	}

	// The merged state is a JSON object keyed by IP. Decode generically so we
	// can optionally filter by reason and render a compact list.
	var merged struct {
		Timestamp string                     `json:"timestamp"`
		States    map[string]json.RawMessage `json:"states"`
	}
	if err := json.Unmarshal(body, &merged); err != nil {
		// Shape differs from expectation; fall back to raw output.
		printRaw(body)
		return exitOK
	}

	if ctx.opts.json && reason == "" {
		printRaw(body)
		return exitOK
	}

	type stateEntry struct {
		Reason     string `json:"reason"`
		State      string `json:"state"`
		ExpireTime string `json:"expire_time"`
	}

	count := 0
	for ip, raw := range merged.States {
		var e stateEntry
		_ = json.Unmarshal(raw, &e)
		if reason != "" && !strings.Contains(e.Reason, reason) {
			continue
		}
		count++
		if ctx.opts.json {
			continue
		}
		fmt.Printf("%-40s %-10s %s\n", ip, e.State, e.Reason)
	}
	if !ctx.opts.json {
		fmt.Printf("\n%d IP(s)", count)
		if reason != "" {
			fmt.Printf(" matching reason %q", reason)
		}
		fmt.Println(".")
	}
	return exitOK
}

func cmdEndpoints(ctx *cmdContext) int {
	if len(ctx.args) != 0 {
		return usageErr("usage: botctl endpoints")
	}
	body, err := ctx.c.do("GET", "/api/v1/help", nil)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	var eps []struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description"`
		Role        string `json:"role"`
	}
	if json.Unmarshal(body, &eps) != nil {
		printRaw(body)
		return exitOK
	}
	for _, e := range eps {
		fmt.Printf("%-7s %-48s %s\n", e.Method, e.Path, e.Description)
	}
	return exitOK
}
