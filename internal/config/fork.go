package config

import "gopkg.in/yaml.v3"

// ValidateFork checks the settings this fork adds on top of upstream: the quota-aware chat
// affinity profile and the ordered strategy's routing order
func (cfg *Config) ValidateFork() error {
	if _, errValidate := cfg.QuotaAffinitySettings(); errValidate != nil {
		return errValidate
	}
	return cfg.ValidateRoutingOrder()
}

// finishForkLoad applies fork overrides to a config parsed from YAML, then validates the
// fork's settings
func (cfg *Config) finishForkLoad() error {
	// session-sticky routing is always on in this fork: prompt caches and account-bound thinking
	// blocks do not carry across credentials, so routing.session-affinity cannot disable it.
	// Only parsed configs are forced; the runtime still honors false for configs built in code
	cfg.Routing.SessionAffinity = true
	return cfg.ValidateFork()
}

// forkKnownDefault reports whether a node is a default that saves should not write, for the
// paths whose defaults this fork changes; ok is false for every other path
func forkKnownDefault(path []string, node *yaml.Node) (known bool, ok bool) {
	// zero disables transient retries, so it must survive both leaf and parent pruning
	if len(path) == 2 && path[0] == "routing" && node != nil && node.Kind == yaml.ScalarNode {
		switch {
		case path[1] == "transient-retries" && node.Tag == "!!int":
			return node.Value == "2", true
		case path[1] == "session-affinity" && node.Tag == "!!bool":
			// the forced value must not be written into files that never set it
			return true, true
		}
	}

	if len(path) == 1 && path[0] == "routing" && node != nil && node.Kind == yaml.MappingNode && findMapKeyIndex(node, "transient-retries") >= 0 {
		return false, true
	}
	return false, false
}
