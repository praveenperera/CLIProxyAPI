package config

import (
	"fmt"
	"strings"
)

// RoutingStrategyOrdered ranks credentials for new sessions by Routing.Order
const RoutingStrategyOrdered = "ordered"

// Routing order criteria; earlier criteria decide and later ones break ties
const (
	// RoutingOrderPriority prefers the credential with the higher priority field
	RoutingOrderPriority = "priority"
	// RoutingOrderWeeklyReset prefers the credential whose weekly quota window resets soonest
	RoutingOrderWeeklyReset = "weekly-reset"
	// RoutingOrderSessionReset prefers the credential whose short (Claude five-hour) window resets soonest
	RoutingOrderSessionReset = "session-reset"
	// RoutingOrderWeeklyRemaining prefers the most weekly quota left, scaled by quota_weight
	RoutingOrderWeeklyRemaining = "weekly-remaining"
	// RoutingOrderSessionRemaining prefers the most short-window quota left, scaled by quota_weight
	RoutingOrderSessionRemaining = "session-remaining"
)

// RoutingOrderCriteria lists every supported routing order criterion
var RoutingOrderCriteria = []string{
	RoutingOrderPriority,
	RoutingOrderWeeklyReset,
	RoutingOrderSessionReset,
	RoutingOrderWeeklyRemaining,
	RoutingOrderSessionRemaining,
}

// DefaultRoutingOrder applies when the ordered strategy has no explicit order
var DefaultRoutingOrder = []string{RoutingOrderWeeklyReset, RoutingOrderSessionRemaining, RoutingOrderPriority}

// NormalizeRoutingOrder lowercases and validates an order, rejecting unknown or repeated
// criteria; an empty order returns DefaultRoutingOrder
func NormalizeRoutingOrder(order []string) ([]string, error) {
	if len(order) == 0 {
		return append([]string(nil), DefaultRoutingOrder...), nil
	}
	normalized := make([]string, 0, len(order))
	seen := make(map[string]bool, len(order))
	for _, raw := range order {
		criterion := strings.ToLower(strings.TrimSpace(raw))
		known := false
		for _, supported := range RoutingOrderCriteria {
			known = known || criterion == supported
		}
		if !known {
			return nil, fmt.Errorf("routing.order: unknown criterion %q", raw)
		}
		if seen[criterion] {
			return nil, fmt.Errorf("routing.order: %q is listed twice", criterion)
		}
		seen[criterion] = true
		normalized = append(normalized, criterion)
	}
	return normalized, nil
}

// ValidateRoutingOrder rejects an invalid routing.order
func (cfg *Config) ValidateRoutingOrder() error {
	if cfg == nil {
		return nil
	}
	_, err := NormalizeRoutingOrder(cfg.Routing.Order)
	return err
}
