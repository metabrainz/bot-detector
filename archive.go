package main

import (
	"bot-detector/internal/types"
	"fmt"
	"time"
)

// GetConfigForArchive provides a thread-safe way to access the data needed for the config archive.
func (p *Processor) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()

	if p.Config == nil {
		return nil, time.Time{}, nil, "", fmt.Errorf("configuration is not available")
	}

	// Create a deep copy of the dependencies map to ensure thread safety.
	depsCopy := make(map[string]*types.FileDependency)
	for k, v := range p.Config.FileDependencies {
		depsCopy[k] = v.Clone()
	}

	return p.Config.YAMLContent, p.Config.LastModTime, depsCopy, p.ConfigPath, nil
}
