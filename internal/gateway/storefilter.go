package gateway

import (
	"strings"

	"wa-gateway/internal/config"
)

// chatFilter decides whether a conversation's messages should be persisted,
// based on optional allow/deny lists of numbers or JIDs from config. This lets
// the gateway store history only for selected numbers/groups.
type chatFilter struct {
	allow map[string]bool
	deny  map[string]bool
	cc    string
}

func newChatFilter(cfg *config.Config) *chatFilter {
	return &chatFilter{
		allow: normalizeChatSet(cfg.StoreChats, cfg.DefaultCountryCode),
		deny:  normalizeChatSet(cfg.StoreChatsExclude, cfg.DefaultCountryCode),
		cc:    cfg.DefaultCountryCode,
	}
}

// normalizeChatSet builds a lookup set from raw entries. Full JIDs (containing
// '@', e.g. groups "...@g.us") are indexed as-is plus their user part; bare
// numbers are normalized to international digits.
func normalizeChatSet(list []string, cc string) map[string]bool {
	m := make(map[string]bool)
	for _, e := range list {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if i := strings.IndexByte(e, '@'); i >= 0 {
			m[e] = true
			m[e[:i]] = true
			continue
		}
		if n, err := NormalizePhone(e, cc); err == nil {
			m[n] = true
		} else {
			m[e] = true
		}
	}
	return m
}

// allowChat reports whether messages for the given chat JID should be stored.
// An allowlist (StoreChats) takes precedence; otherwise a denylist
// (StoreChatsExclude) applies; with neither configured, everything is stored.
func (f *chatFilter) allowChat(chatJID string) bool {
	full := chatJID
	user := chatJID
	if i := strings.IndexByte(chatJID, '@'); i >= 0 {
		user = chatJID[:i]
	}
	if n, err := NormalizePhone(user, f.cc); err == nil {
		user = n
	}
	if len(f.allow) > 0 {
		return f.allow[full] || f.allow[user]
	}
	if len(f.deny) > 0 {
		return !(f.deny[full] || f.deny[user])
	}
	return true
}
