package main

import (
	"encoding/json"
	"fmt"
)

// ipStatus models the union of fields returned by GET /api/v1/ip/{ip} for both
// standalone and cluster responses. Fields are optional; only those relevant to
// the response shape are populated.
type ipStatus struct {
	// Standalone
	Node              string            `json:"node,omitempty"`
	Status            string            `json:"status,omitempty"`
	Actors            int               `json:"actors,omitempty"`
	Chains            map[string]string `json:"chains,omitempty"`
	EarliestBlock     string            `json:"earliest_block,omitempty"`
	LatestExpiry      string            `json:"latest_expiry,omitempty"`
	Persistence       string            `json:"persistence,omitempty"`
	PersistenceExpiry string            `json:"persistence_expires,omitempty"`
	PersistenceReason string            `json:"persistence_reason,omitempty"`
	LastSeen          string            `json:"last_seen,omitempty"`
	LastUnblock       string            `json:"last_unblock,omitempty"`
	UnblockReason     string            `json:"unblock_reason,omitempty"`
	BadActor          *struct {
		PromotedAt string  `json:"promoted_at"`
		TotalScore float64 `json:"total_score"`
		BlockCount int     `json:"block_count"`
		History    string  `json:"history"`
	} `json:"bad_actor,omitempty"`
	Score *struct {
		CurrentScore float64 `json:"current_score"`
		BlockCount   int     `json:"block_count"`
		Threshold    float64 `json:"threshold"`
	} `json:"score,omitempty"`

	// Cluster
	ClusterStatus string `json:"cluster_status,omitempty"`
	Nodes         []struct {
		Name              string            `json:"name"`
		Status            string            `json:"status"`
		Actors            int               `json:"actors,omitempty"`
		Chains            map[string]string `json:"chains,omitempty"`
		Persistence       string            `json:"persistence,omitempty"`
		PersistenceExpiry string            `json:"persistence_expires,omitempty"`
	} `json:"nodes,omitempty"`
}

// effectiveStatus returns the block status regardless of standalone/cluster
// shape: "blocked", "unblocked", or "unknown". For cluster responses, a
// "mixed" cluster_status (or any node reporting blocked/persistence-blocked) is
// treated as "blocked", since the IP is blocked somewhere in the cluster.
func (s *ipStatus) effectiveStatus() string {
	if s.ClusterStatus != "" {
		if s.ClusterStatus == "mixed" || s.anyNodeBlocked() {
			return "blocked"
		}
		return s.ClusterStatus
	}
	if s.Status != "" {
		return s.Status
	}
	return "unknown"
}

// anyNodeBlocked reports whether any cluster node has the IP blocked (either
// live status or persisted state).
func (s *ipStatus) anyNodeBlocked() bool {
	for _, n := range s.Nodes {
		if n.Status == "blocked" || n.Persistence == "blocked" {
			return true
		}
	}
	return false
}

// fetchIPStatus retrieves and decodes the status of an IP.
func (ctx *cmdContext) fetchIPStatus(ip string) (*ipStatus, []byte, error) {
	var st ipStatus
	body, err := ctx.c.doJSON("GET", "/api/v1/ip/"+ip, nil, &st)
	if err != nil {
		return nil, body, err
	}
	return &st, body, nil
}

// renderIPStatus prints a human-readable summary of an IP status.
func renderIPStatus(ip string, s *ipStatus) {
	// Show the server's own status label (which may be "mixed" for clusters);
	// the exit code separately uses effectiveStatus().
	label := s.Status
	if s.ClusterStatus != "" {
		label = s.ClusterStatus
	}
	if label == "" {
		label = "unknown"
	}
	fmt.Printf("IP:     %s\n", ip)
	fmt.Printf("Status: %s\n", label)

	if len(s.Nodes) > 0 {
		fmt.Println("Nodes:")
		for _, n := range s.Nodes {
			fmt.Printf("  - %-16s %s\n", n.Name, n.Status)
			if n.Persistence != "" {
				fmt.Printf("      persistence: %s", n.Persistence)
				if n.PersistenceExpiry != "" {
					fmt.Printf(" (expires %s)", n.PersistenceExpiry)
				}
				fmt.Println()
			}
			for chain, exp := range n.Chains {
				fmt.Printf("      chain: %s (expires %s)\n", chain, exp)
			}
		}
		return
	}

	if s.PersistenceReason != "" {
		fmt.Printf("Reason: %s\n", s.PersistenceReason)
	}
	if s.PersistenceExpiry != "" {
		fmt.Printf("Expires: %s\n", s.PersistenceExpiry)
	}
	for chain, exp := range s.Chains {
		fmt.Printf("Chain:  %s (expires %s)\n", chain, exp)
	}
	if s.BadActor != nil {
		fmt.Printf("Bad actor: promoted %s, score %.1f, blocks %d\n",
			s.BadActor.PromotedAt, s.BadActor.TotalScore, s.BadActor.BlockCount)
	}
	if s.Score != nil {
		fmt.Printf("Score:  %.2f / %.2f (blocks %d)\n",
			s.Score.CurrentScore, s.Score.Threshold, s.Score.BlockCount)
	}
	if s.UnblockReason != "" {
		fmt.Printf("Unblock reason: %s\n", s.UnblockReason)
	}
	if s.LastSeen != "" {
		fmt.Printf("Last seen: %s\n", s.LastSeen)
	}
}

// statusExitCode maps a block status to a process exit code for `ip check`.
func statusExitCode(status string) int {
	switch status {
	case "blocked":
		return exitBlocked
	case "unknown", "":
		return exitNotFound
	default:
		return exitOK
	}
}

func cmdIPCheck(ctx *cmdContext) int {
	if len(ctx.args) != 1 {
		return usageErr("usage: botctl ip check <ip>")
	}
	ip, err := validateIP(ctx.args[0])
	if err != nil {
		return fail(err)
	}

	st, body, err := ctx.fetchIPStatus(ip)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
	} else {
		renderIPStatus(ip, st)
	}
	return statusExitCode(st.effectiveStatus())
}

func cmdIPUnblock(ctx *cmdContext) int {
	if len(ctx.args) != 1 {
		return usageErr("usage: botctl ip unblock <ip>")
	}
	ip, err := validateIP(ctx.args[0])
	if err != nil {
		return fail(err)
	}

	// Check current status first — this is the common "check, then unblock if
	// blocked" workflow. If not blocked, there's nothing to do.
	st, _, err := ctx.fetchIPStatus(ip)
	if err != nil {
		return fail(err)
	}
	status := st.effectiveStatus()
	if !ctx.opts.json {
		renderIPStatus(ip, st)
	}
	if status != "blocked" {
		fmt.Printf("%s is not blocked (status: %s); nothing to do.\n", ip, status)
		return exitOK
	}

	if !confirm(ctx.opts, fmt.Sprintf("Unblock %s?", ip)) {
		eprintln("aborted.")
		return exitOK
	}

	var res struct {
		IP     string `json:"ip"`
		Status string `json:"status"`
	}
	body, err := ctx.c.doJSON("POST", "/api/v1/ip/"+ip+"/unblock", nil, &res)
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

func cmdIPClear(ctx *cmdContext) int {
	if len(ctx.args) != 1 {
		return usageErr("usage: botctl ip clear <ip>")
	}
	ip, err := validateIP(ctx.args[0])
	if err != nil {
		return fail(err)
	}

	if !confirm(ctx.opts, fmt.Sprintf("Clear %s from all state?", ip)) {
		eprintln("aborted.")
		return exitOK
	}

	body, err := ctx.c.do("POST", "/api/v1/ip/"+ip+"/clear", nil)
	if err != nil {
		return fail(err)
	}
	if ctx.opts.json {
		printRaw(body)
		return exitOK
	}
	// The clear response is JSON; pretty-print a short confirmation.
	var generic map[string]any
	if json.Unmarshal(body, &generic) == nil && len(generic) > 0 {
		printJSON(generic)
	} else {
		fmt.Printf("%s cleared.\n", ip)
	}
	return exitOK
}
