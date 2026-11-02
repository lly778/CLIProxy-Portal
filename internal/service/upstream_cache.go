package service

// InvalidateUpstreamReadCache refreshes the read-only upstream management view.
// Local derived views also invalidate for alternate clients without a raw-response cache.
func (k *Keys) InvalidateUpstreamReadCache() {
	if client, ok := k.CPAMP.(interface{ InvalidateUpstreamReadCache() }); ok {
		client.InvalidateUpstreamReadCache()
	}
	k.upstreamViews.mu.Lock()
	k.upstreamViews.generation++
	k.upstreamViews.entries = nil
	k.upstreamViews.mu.Unlock()
	k.clientCatalog.mu.Lock()
	k.clientCatalog.localGeneration++
	k.clientCatalog.value = nil
	k.clientCatalog.mu.Unlock()
}
