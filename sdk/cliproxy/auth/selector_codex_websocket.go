package auth

// allCodexAuths reports whether a mixed-provider pick only has Codex candidates, as when
// session affinity routes a single-provider Codex request through the mixed legacy path
func allCodexAuths(auths []*Auth) bool {
	for _, a := range auths {
		if canonicalSchedulingProvider(executorKeyFromAuth(a)) != "codex" {
			return false
		}
	}
	return true
}
