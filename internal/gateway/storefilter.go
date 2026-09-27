package gateway

import (
	"strings"

	"wa-gateway/internal/config"
)

// chatFilter decides whether a conversation's messages should be persisted,
// based on optional allow/deny lists of numbers or JIDs from config, plus the
// runtime per-chat opt-ins (chatArchive). This lets the gateway store history
// only for selected numbers/groups.
type chatFilter struct {
	allow   map[string]bool
	deny    map[string]bool
	cc      string
	global  bool // STORE_MESSAGES
	archive *chatArchive
}

func newChatFilter(cfg *config.Config, archive *chatArchive) *chatFilter {
	return &chatFilter{
		allow:   normalizeChatSet(cfg.StoreChats, cfg.DefaultCountryCode),
		deny:    normalizeChatSet(cfg.StoreChatsExclude, cfg.DefaultCountryCode),
		cc:      cfg.DefaultCountryCode,
		global:  cfg.StoreMessages,
		archive: archive,
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
// A runtime opt-in (chatArchive) always wins. Otherwise STORE_MESSAGES must be
// on, then an allowlist (StoreChats) takes precedence, else a denylist
// (StoreChatsExclude) applies; with neither configured, everything is stored.
func (f *chatFilter) allowChat(session, chatJID string) bool {
	if f.archive != nil && f.archive.enabled(session, chatJID) {
		return true
	}
	if !f.global {
		return false
	}
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
