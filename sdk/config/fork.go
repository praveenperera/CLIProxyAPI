package config

import internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"

// QuotaAffinityConfig configures the optional local routing profile
type QuotaAffinityConfig = internalconfig.QuotaAffinityConfig

// QuotaAffinitySettings contains validated profile settings
type QuotaAffinitySettings = internalconfig.QuotaAffinitySettings

// RoutingStrategyOrdered ranks credentials for new sessions by Routing.Order
const RoutingStrategyOrdered = internalconfig.RoutingStrategyOrdered

// NormalizeRoutingOrder validates an ordered-strategy order; empty returns the default order
func NormalizeRoutingOrder(order []string) ([]string, error) {
	return internalconfig.NormalizeRoutingOrder(order)
}
