package api

// ResolvePodOwner walks the owner chain for a pod (Pod → ReplicaSet →
// Deployment, etc.) using the in-memory cluster cache. Returns the top-level
// owner Kind/Name, or ("Pod", podName) if no owner can be resolved.
func ResolvePodOwner(ns, podName string) (string, string) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if cache.pods == nil {
		return "Pod", podName
	}
	for _, p := range cache.pods {
		if p.Namespace != ns || p.Name != podName {
			continue
		}
		if len(p.OwnerReferences) == 0 {
			return "Pod", podName
		}
		ownerKind := p.OwnerReferences[0].Kind
		ownerName := p.OwnerReferences[0].Name
		if ownerKind == "ReplicaSet" && cache.replicasets != nil {
			for _, rs := range cache.replicasets {
				if rs.Name == ownerName && rs.Namespace == ns && len(rs.OwnerReferences) > 0 {
					ownerKind = rs.OwnerReferences[0].Kind
					ownerName = rs.OwnerReferences[0].Name
					break
				}
			}
		}
		return ownerKind, ownerName
	}
	return "Pod", podName
}

// resolvePodOwner is the lower-case alias used by handlers inside this
// package. Kept as a thin alias to minimize the diff during the move; the
// public surface is ResolvePodOwner.
func resolvePodOwner(ns, podName string) (string, string) {
	return ResolvePodOwner(ns, podName)
}
