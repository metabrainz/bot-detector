package app

import (
	"bot-detector/internal/config"
)

// BuildVHostMap creates a mapping from vhost to website name.
func BuildVHostMap(websites []config.WebsiteConfig) map[string]string {
	m := make(map[string]string)
	for _, ws := range websites {
		for _, vhost := range ws.VHosts {
			m[vhost] = ws.Name
		}
	}
	return m
}

// CategorizeChains separates chains into website-specific and global chains.
// Returns:
//   - websiteChains: map of website name to slice of chain indices
//   - globalChains: slice of indices for chains that apply to all websites
func CategorizeChains(chains []config.BehavioralChain) (map[string][]int, []int) {
	websiteChains := make(map[string][]int)
	var globalChains []int

	for i, chain := range chains {
		if len(chain.Websites) == 0 {
			// No website filter = applies globally
			globalChains = append(globalChains, i)
		} else {
			// Add to each specified website
			for _, ws := range chain.Websites {
				websiteChains[ws] = append(websiteChains[ws], i)
			}
		}
	}

	return websiteChains, globalChains
}
