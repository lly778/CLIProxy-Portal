package service

// InvalidateUpstreamReadCache refreshes the read-only upstream management view.
// Test and alternate clients without this optional cache need no invalidation.
func (k *Keys) InvalidateUpstreamReadCache() {
	if client, ok := k.CPAMP.(interface{ InvalidateUpstreamReadCache() }); ok {
		client.InvalidateUpstreamReadCache()
	}
}
