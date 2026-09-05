package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Batas WhatsApp: nama grup maks 25 karakter; peserta per pembuatan dibatasi
// agar akun bot tidak dianggap spam (WA sendiri mengizinkan hingga 1024).
const (
	maxGroupNameLen        = 25
	maxGroupParticipants   = 256
	participantAddPace     = 1500 * time.Millisecond
	participantAddBatchLen = 20
)

// ParticipantStatus adalah hasil per-peserta saat create/add.
//
// Status:
//   - added    : peserta langsung masuk grup
//   - invited  : privasi peserta menolak penambahan langsung (WA 403). WA
//     memberi kode undangan khusus; klien harus mengirim tautan undangan.
//   - exists   : sudah anggota (WA 409)
//   - failed   : gagal lain (lihat Error/Code)
type ParticipantStatus struct {
	Phone      string `json:"phone"`
	JID        string `json:"jid,omitempty"`
	Status     string `json:"status"`
	Code       int    `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	InviteCode string `json:"inviteCode,omitempty"`
}

// GroupDetail adalah metadata grup beserta daftar anggota.
type GroupDetail struct {
	JID          string            `json:"jid"`
	Name         string            `json:"name"`
	Topic        string            `json:"topic,omitempty"`
	OwnerJID     string            `json:"ownerJid,omitempty"`
	IsAnnounce   bool              `json:"isAnnounce"`
	IsLocked     bool              `json:"isLocked"`
	CreatedAt    int64             `json:"createdAt,omitempty"`
	Participants []GroupMember     `json:"participants"`
	InviteLink   string            `json:"inviteLink,omitempty"`
	Extra        map[string]string `json:"-"`
}

// GroupMember adalah satu anggota grup.
type GroupMember struct {
	JID          string `json:"jid"`
	Phone        string `json:"phone,omitempty"`
	IsAdmin      bool   `json:"isAdmin"`
	IsSuperAdmin bool   `json:"isSuperAdmin"`
}

// CreateGroupInput adalah parameter pembuatan grup.
type CreateGroupInput struct {
	Name         string
	Topic        string
	Participants []string
	// Announce: hanya admin yang bisa mengirim pesan.
	Announce bool
	// Locked: hanya admin yang bisa mengubah info grup.
	Locked bool
}

// CreateGroupResult adalah hasil pembuatan grup.
type CreateGroupResult struct {
	JID          string              `json:"jid"`
	Name         string              `json:"name"`
	InviteLink   string              `json:"inviteLink,omitempty"`
	Participants []ParticipantStatus `json:"participants"`
}

// CreateGroup membuat grup baru. Peserta dimasukkan bertahap (paced) supaya
// penambahan massal tidak memicu pembatasan akun. Peserta yang menolak
// penambahan langsung (privasi) dilaporkan berstatus "invited" beserta kode
// undangan — pemanggil bertanggung jawab mengirimkan tautan undangan.
func (s *Session) CreateGroup(ctx context.Context, in CreateGroupInput) (*CreateGroupResult, error) {
	if !s.wa.IsLoggedIn() {
		return nil, errors.New("session is not logged in")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errors.New("group name is required")
	}
	if len([]rune(name)) > maxGroupNameLen {
		return nil, fmt.Errorf("group name too long (max %d characters)", maxGroupNameLen)
	}
	if len(in.Participants) > maxGroupParticipants {
		return nil, fmt.Errorf("too many participants (max %d)", maxGroupParticipants)
	}

	jids, statuses, err := s.resolveParticipants(in.Participants)
	if err != nil {
		return nil, err
	}

	// Buat grup dengan batch pertama; sisanya ditambahkan bertahap.
	first := jids
	rest := []types.JID(nil)
	if len(jids) > participantAddBatchLen {
		first = jids[:participantAddBatchLen]
		rest = jids[participantAddBatchLen:]
	}
	req := whatsmeow.ReqCreateGroup{Name: name, Participants: first}
	req.GroupAnnounce.IsAnnounce = in.Announce
	req.GroupLocked.IsLocked = in.Locked
	info, err := s.wa.CreateGroup(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create group: %w", err)
	}
	s.log.Infof("Group created %s (%s) with %d initial participants", info.JID, name, len(first))
	applyParticipantResults(statuses, info.Participants, first)

	if rest != nil {
		s.addParticipantsPaced(ctx, info.JID, rest, statuses)
	}

	if t := strings.TrimSpace(in.Topic); t != "" {
		if err := s.wa.SetGroupTopic(ctx, info.JID, "", "", t); err != nil {
			s.log.Warnf("Set group topic %s: %v", info.JID, err)
		}
	}

	res := &CreateGroupResult{JID: info.JID.String(), Name: name, Participants: statusList(statuses, in.Participants)}
	if link, err := s.wa.GetGroupInviteLink(ctx, info.JID, false); err == nil {
		res.InviteLink = link
	} else {
		s.log.Warnf("Get invite link %s: %v", info.JID, err)
	}
	return res, nil
}

// AddParticipants menambahkan peserta ke grup yang sudah ada (paced).
func (s *Session) AddParticipants(ctx context.Context, group string, phones []string) ([]ParticipantStatus, error) {
	if !s.wa.IsLoggedIn() {
		return nil, errors.New("session is not logged in")
	}
	gjid, err := parseGroupJID(group)
	if err != nil {
		return nil, err
	}
	if len(phones) > maxGroupParticipants {
		return nil, fmt.Errorf("too many participants (max %d)", maxGroupParticipants)
	}
	jids, statuses, err := s.resolveParticipants(phones)
	if err != nil {
		return nil, err
	}
	s.addParticipantsPaced(ctx, gjid, jids, statuses)
	return statusList(statuses, phones), nil
}

// RemoveParticipants mengeluarkan peserta dari grup.
func (s *Session) RemoveParticipants(ctx context.Context, group string, phones []string) ([]ParticipantStatus, error) {
	if !s.wa.IsLoggedIn() {
		return nil, errors.New("session is not logged in")
	}
	gjid, err := parseGroupJID(group)
	if err != nil {
		return nil, err
	}
	jids, statuses, err := s.resolveParticipants(phones)
	if err != nil {
		return nil, err
	}
	res, err := s.wa.UpdateGroupParticipants(ctx, gjid, jids, whatsmeow.ParticipantChangeRemove)
	if err != nil {
		return nil, fmt.Errorf("remove participants: %w", err)
	}
	for _, p := range res {
		st := statuses[p.JID.User]
		if st == nil && !p.PhoneNumber.IsEmpty() {
			st = statuses[p.PhoneNumber.User]
		}
		if st == nil {
			continue
		}
		if p.Error == 0 {
			st.Status = "removed"
		} else {
			st.Status = "failed"
			st.Code = p.Error
			st.Error = participantErrorText(p.Error)
		}
	}
	return statusList(statuses, phones), nil
}

// GroupInfo mengambil metadata + anggota grup. Tautan undangan disertakan bila
// withInvite true (butuh hak admin di grup).
func (s *Session) GroupDetail(ctx context.Context, group string, withInvite bool) (*GroupDetail, error) {
	if !s.wa.IsLoggedIn() {
		return nil, errors.New("session is not logged in")
	}
	gjid, err := parseGroupJID(group)
	if err != nil {
		return nil, err
	}
	info, err := s.wa.GetGroupInfo(ctx, gjid)
	if err != nil {
		return nil, fmt.Errorf("get group info: %w", err)
	}
	d := &GroupDetail{
		JID:        info.JID.String(),
		Name:       info.GroupName.Name,
		Topic:      info.GroupTopic.Topic,
		OwnerJID:   info.OwnerJID.String(),
		IsAnnounce: info.GroupAnnounce.IsAnnounce,
		IsLocked:   info.GroupLocked.IsLocked,
	}
	if !info.GroupCreated.IsZero() {
		d.CreatedAt = info.GroupCreated.Unix()
	}
	d.Participants = make([]GroupMember, 0, len(info.Participants))
	for _, p := range info.Participants {
		m := GroupMember{JID: p.JID.String(), IsAdmin: p.IsAdmin, IsSuperAdmin: p.IsSuperAdmin}
		if !p.PhoneNumber.IsEmpty() {
			m.Phone = p.PhoneNumber.User
		} else if p.JID.Server == types.DefaultUserServer {
			m.Phone = p.JID.User
		}
		d.Participants = append(d.Participants, m)
	}
	if withInvite {
		if link, err := s.wa.GetGroupInviteLink(ctx, gjid, false); err == nil {
			d.InviteLink = link
		}
	}
	return d, nil
}

// GroupInviteLink mengambil (atau mereset) tautan undangan grup.
func (s *Session) GroupInviteLink(ctx context.Context, group string, reset bool) (string, error) {
	if !s.wa.IsLoggedIn() {
		return "", errors.New("session is not logged in")
	}
	gjid, err := parseGroupJID(group)
	if err != nil {
		return "", err
	}
	link, err := s.wa.GetGroupInviteLink(ctx, gjid, reset)
	if err != nil {
		return "", fmt.Errorf("get invite link: %w", err)
	}
	return link, nil
}

// UpdateGroupSettings mengubah nama/topik/announce/locked. Field nil = tidak diubah.
type UpdateGroupInput struct {
	Name     *string
	Topic    *string
	Announce *bool
	Locked   *bool
}

func (s *Session) UpdateGroup(ctx context.Context, group string, in UpdateGroupInput) error {
	if !s.wa.IsLoggedIn() {
		return errors.New("session is not logged in")
	}
	gjid, err := parseGroupJID(group)
	if err != nil {
		return err
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len([]rune(n)) > maxGroupNameLen {
			return fmt.Errorf("group name must be 1-%d characters", maxGroupNameLen)
		}
		if err := s.wa.SetGroupName(ctx, gjid, n); err != nil {
			return fmt.Errorf("set name: %w", err)
		}
	}
	if in.Topic != nil {
		if err := s.wa.SetGroupTopic(ctx, gjid, "", "", strings.TrimSpace(*in.Topic)); err != nil {
			return fmt.Errorf("set topic: %w", err)
		}
	}
	if in.Announce != nil {
		if err := s.wa.SetGroupAnnounce(ctx, gjid, *in.Announce); err != nil {
			return fmt.Errorf("set announce: %w", err)
		}
	}
	if in.Locked != nil {
		if err := s.wa.SetGroupLocked(ctx, gjid, *in.Locked); err != nil {
			return fmt.Errorf("set locked: %w", err)
		}
	}
	return nil
}

// LeaveGroup keluar dari grup (grup tetap ada untuk anggota lain).
func (s *Session) LeaveGroup(ctx context.Context, group string) error {
	if !s.wa.IsLoggedIn() {
		return errors.New("session is not logged in")
	}
	gjid, err := parseGroupJID(group)
	if err != nil {
		return err
	}
	return s.wa.LeaveGroup(ctx, gjid)
}

// ── helpers ─────────────────────────────────────────────────────────────

func parseGroupJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasSuffix(raw, "@g.us") {
		return types.JID{}, errors.New("group jid must end with @g.us")
	}
	jid, err := types.ParseJID(raw)
	if err != nil {
		return types.JID{}, fmt.Errorf("invalid group jid: %w", err)
	}
	return jid, nil
}

// resolveParticipants menormalkan nomor → JID dan menyiapkan peta status.
// Nomor duplikat (setelah normalisasi) digabung. Nomor tak valid langsung
// berstatus failed tanpa menggagalkan seluruh permintaan.
func (s *Session) resolveParticipants(phones []string) ([]types.JID, map[string]*ParticipantStatus, error) {
	statuses := make(map[string]*ParticipantStatus, len(phones))
	jids := make([]types.JID, 0, len(phones))
	seen := make(map[string]bool, len(phones))
	for _, p := range phones {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		jid, err := parseJID(p, s.mgr.cfg.DefaultCountryCode)
		if err != nil || jid.Server != types.DefaultUserServer {
			msg := "invalid phone"
			if err != nil {
				msg = err.Error()
			}
			statuses["raw:"+p] = &ParticipantStatus{Phone: p, Status: "failed", Error: msg}
			continue
		}
		if seen[jid.User] {
			continue
		}
		seen[jid.User] = true
		statuses[jid.User] = &ParticipantStatus{Phone: p, JID: jid.String(), Status: "pending"}
		jids = append(jids, jid)
	}
	if len(jids) == 0 && len(statuses) == 0 {
		return nil, nil, errors.New("participants is required")
	}
	return jids, statuses, nil
}

// addParticipantsPaced menambahkan peserta per batch dengan jeda antar batch.
func (s *Session) addParticipantsPaced(ctx context.Context, gjid types.JID, jids []types.JID, statuses map[string]*ParticipantStatus) {
	for i := 0; i < len(jids); i += participantAddBatchLen {
		end := i + participantAddBatchLen
		if end > len(jids) {
			end = len(jids)
		}
		batch := jids[i:end]
		if i > 0 {
			select {
			case <-ctx.Done():
				markPending(statuses, batch, "cancelled")
				return
			case <-time.After(participantAddPace):
			}
		}
		res, err := s.wa.UpdateGroupParticipants(ctx, gjid, batch, whatsmeow.ParticipantChangeAdd)
		if err != nil {
			s.log.Warnf("Add participants to %s: %v", gjid, err)
			markPending(statuses, batch, err.Error())
			continue
		}
		applyParticipantResults(statuses, res, batch)
	}
}

func markPending(statuses map[string]*ParticipantStatus, batch []types.JID, reason string) {
	for _, j := range batch {
		if st := statuses[j.User]; st != nil && st.Status == "pending" {
			st.Status = "failed"
			st.Error = reason
		}
	}
}

// applyParticipantResults memetakan hasil WA ke status. Peserta dalam batch
// yang tidak muncul di hasil dianggap berhasil (perilaku CreateGroup: hanya
// peserta bermasalah yang membawa Error).
func applyParticipantResults(statuses map[string]*ParticipantStatus, res []types.GroupParticipant, batch []types.JID) {
	for _, p := range res {
		st := statuses[p.JID.User]
		if st == nil && !p.PhoneNumber.IsEmpty() {
			st = statuses[p.PhoneNumber.User]
		}
		if st == nil {
			continue
		}
		switch {
		case p.Error == 0:
			st.Status = "added"
		case p.Error == 403:
			st.Status = "invited"
			st.Code = p.Error
			st.Error = participantErrorText(p.Error)
			if p.AddRequest != nil {
				st.InviteCode = p.AddRequest.Code
			}
		case p.Error == 409:
			st.Status = "exists"
			st.Code = p.Error
		default:
			st.Status = "failed"
			st.Code = p.Error
			st.Error = participantErrorText(p.Error)
		}
	}
	for _, j := range batch {
		if st := statuses[j.User]; st != nil && st.Status == "pending" {
			st.Status = "added"
		}
	}
}

func participantErrorText(code int) string {
	switch code {
	case 401:
		return "participant blocked this account"
	case 403:
		return "privacy settings prevent direct add; invite required"
	case 404:
		return "not a WhatsApp user"
	case 408:
		return "participant left the group recently"
	case 409:
		return "already a participant"
	case 500:
		return "group is full"
	default:
		return fmt.Sprintf("whatsapp error %d", code)
	}
}

// statusList mengembalikan status sesuai urutan input.
func statusList(statuses map[string]*ParticipantStatus, phones []string) []ParticipantStatus {
	out := make([]ParticipantStatus, 0, len(statuses))
	emitted := make(map[*ParticipantStatus]bool, len(statuses))
	for _, p := range phones {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var st *ParticipantStatus
		if raw := statuses["raw:"+p]; raw != nil {
			st = raw
		} else {
			for _, v := range statuses {
				if v.Phone == p {
					st = v
					break
				}
			}
		}
		if st == nil || emitted[st] {
			continue
		}
		emitted[st] = true
		out = append(out, *st)
	}
	return out
}
