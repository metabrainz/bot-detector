package main

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// challengeStatus models GET /api/v1/challenge/{ip}.
// difficulty: -1 not challenged, 0 challenged with frontend default, N specific.
type challengeStatus struct {
	IP         string `json:"ip"`
	Difficulty int    `json:"difficulty"`
}

func cmdChallengeCheck(ctx *cmdContext) int {
	if len(ctx.args) != 1 {
		return usageErr("usage: botctl challenge check <ip>")
	}
	ip, err := validateIP(ctx.args[0])
	if err != nil {
		return fail(err)
	}

	var st challengeStatus
	body, err := ctx.c.doJSON("GET", "/api/v1/challenge/"+ip, nil, &st)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
	} else if st.Difficulty < 0 {
		fmt.Printf("%s: not challenged\n", ip)
	} else {
		fmt.Printf("%s: challenged (difficulty %d)\n", ip, st.Difficulty)
	}
	// Exit code: 2 if challenged, 0 if not — mirrors `ip check` semantics.
	if st.Difficulty >= 0 {
		return exitBlocked
	}
	return exitOK
}

func cmdChallengeSet(ctx *cmdContext) int {
	durStr, rest, err := extractStringFlag(ctx.args, "--duration")
	if err != nil {
		return usageErr(err.Error())
	}
	diffStr, rest, err := extractStringFlag(rest, "--difficulty")
	if err != nil {
		return usageErr(err.Error())
	}
	if len(rest) != 1 {
		return usageErr("usage: botctl challenge set <ip> [--duration <dur>] [--difficulty <n>]")
	}
	ip, err := validateIP(rest[0])
	if err != nil {
		return fail(err)
	}

	q := url.Values{}
	if durStr != "" {
		if _, err := time.ParseDuration(durStr); err != nil {
			return usageErr(fmt.Sprintf("invalid --duration %q: %v", durStr, err))
		}
		q.Set("duration", durStr)
	}
	if diffStr != "" {
		n, err := strconv.Atoi(diffStr)
		if err != nil || n < 0 {
			return usageErr(fmt.Sprintf("invalid --difficulty %q (must be a non-negative integer)", diffStr))
		}
		q.Set("difficulty", diffStr)
	}

	if !confirm(ctx.opts, fmt.Sprintf("Challenge %s?", ip)) {
		eprintln("aborted.")
		return exitOK
	}

	var res struct {
		IP         string `json:"ip"`
		Duration   string `json:"duration"`
		Difficulty int    `json:"difficulty"`
		Status     string `json:"status"`
	}
	body, err := ctx.c.doJSON("POST", "/api/v1/challenge/"+ip, q, &res)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	fmt.Printf("%s: %s (duration %s, difficulty %d)\n", res.IP, res.Status, res.Duration, res.Difficulty)
	return exitOK
}

func cmdChallengeRemove(ctx *cmdContext) int {
	if len(ctx.args) != 1 {
		return usageErr("usage: botctl challenge remove <ip>")
	}
	ip, err := validateIP(ctx.args[0])
	if err != nil {
		return fail(err)
	}

	if !confirm(ctx.opts, fmt.Sprintf("Remove challenge for %s?", ip)) {
		eprintln("aborted.")
		return exitOK
	}

	var res struct {
		IP     string `json:"ip"`
		Status string `json:"status"`
	}
	body, err := ctx.c.doJSON("DELETE", "/api/v1/challenge/"+ip, nil, &res)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	fmt.Printf("%s: %s\n", res.IP, res.Status)
	return exitOK
}
