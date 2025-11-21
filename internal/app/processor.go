package app

// InternReason returns a canonical pointer to the reason string.
// This reduces memory usage by ensuring all IPs with the same reason
// share the same underlying string data.
func (p *Processor) InternReason(reason string) string {
	// Fast path: check if already interned (read lock)
	p.ReasonCacheMutex.RLock()
	if canonical, exists := p.ReasonCache[reason]; exists {
		p.ReasonCacheMutex.RUnlock()
		return *canonical
	}
	p.ReasonCacheMutex.RUnlock()

	// Slow path: intern new reason (write lock)
	p.ReasonCacheMutex.Lock()
	defer p.ReasonCacheMutex.Unlock()

	// Double-check after acquiring write lock
	if canonical, exists := p.ReasonCache[reason]; exists {
		return *canonical
	}

	// Create new canonical string
	canonical := reason
	p.ReasonCache[reason] = &canonical
	return canonical
}
