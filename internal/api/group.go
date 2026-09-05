package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"wa-gateway/internal/gateway"
)

// Group management endpoints (scope "group").
//
// Penambahan peserta di-pace oleh gateway, jadi permintaan besar bisa memakan
// waktu ~1.5 detik per 20 peserta — timeout handler disesuaikan.

type createGroupRequest struct {
	Session      string   `json:"session"`
	Name         string   `json:"name"`
	Topic        string   `json:"topic"`
	Participants []string `json:"participants"`
	Announce     bool     `json:"announce"`
	Locked       bool     `json:"locked"`
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "field 'name' is required")
		return
	}
	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res, err := sess.CreateGroup(ctx, gateway.CreateGroupInput{
		Name:         req.Name,
		Topic:        req.Topic,
		Participants: req.Participants,
		Announce:     req.Announce,
		Locked:       req.Locked,
	})
	if err != nil {
		writeError(w, groupErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

type participantsRequest struct {
	Session      string   `json:"session"`
	Participants []string `json:"participants"`
}

func (s *Server) handleAddParticipants(w http.ResponseWriter, r *http.Request) {
	s.handleParticipantChange(w, r, func(sess *gateway.Session, ctx context.Context, jid string, phones []string) ([]gateway.ParticipantStatus, error) {
		return sess.AddParticipants(ctx, jid, phones)
	})
}

func (s *Server) handleRemoveParticipants(w http.ResponseWriter, r *http.Request) {
	s.handleParticipantChange(w, r, func(sess *gateway.Session, ctx context.Context, jid string, phones []string) ([]gateway.ParticipantStatus, error) {
		return sess.RemoveParticipants(ctx, jid, phones)
	})
}

type participantChangeFunc func(sess *gateway.Session, ctx context.Context, jid string, phones []string) ([]gateway.ParticipantStatus, error)

func (s *Server) handleParticipantChange(w http.ResponseWriter, r *http.Request, fn participantChangeFunc) {
	jid := r.PathValue("jid")
	var req participantsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Participants) == 0 {
		writeError(w, http.StatusBadRequest, "field 'participants' is required")
		return
	}
	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res, err := fn(sess, ctx, jid, req.Participants)
	if err != nil {
		writeError(w, groupErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jid": jid, "participants": res})
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(sessionName(r), w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	withInvite := r.URL.Query().Get("invite") == "true"
	d, err := sess.GroupDetail(ctx, r.PathValue("jid"), withInvite)
	if err != nil {
		writeError(w, groupErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleGroupInviteLink(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(sessionName(r), w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	reset := r.URL.Query().Get("reset") == "true"
	link, err := sess.GroupInviteLink(ctx, r.PathValue("jid"), reset)
	if err != nil {
		writeError(w, groupErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"jid": r.PathValue("jid"), "inviteLink": link})
}

type updateGroupRequest struct {
	Session  string  `json:"session"`
	Name     *string `json:"name"`
	Topic    *string `json:"topic"`
	Announce *bool   `json:"announce"`
	Locked   *bool   `json:"locked"`
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	var req updateGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == nil && req.Topic == nil && req.Announce == nil && req.Locked == nil {
		writeError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := sess.UpdateGroup(ctx, r.PathValue("jid"), gateway.UpdateGroupInput{
		Name: req.Name, Topic: req.Topic, Announce: req.Announce, Locked: req.Locked,
	})
	if err != nil {
		writeError(w, groupErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jid": r.PathValue("jid"), "updated": true})
}

func (s *Server) handleLeaveGroup(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(sessionName(r), w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := sess.LeaveGroup(ctx, r.PathValue("jid")); err != nil {
		writeError(w, groupErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jid": r.PathValue("jid"), "left": true})
}

// groupErrStatus memetakan error validasi lokal ke 400, sisanya (WA) ke 502.
func groupErrStatus(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "is required"),
		strings.Contains(msg, "too long"),
		strings.Contains(msg, "too many"),
		strings.Contains(msg, "must end with"),
		strings.Contains(msg, "invalid group jid"),
		strings.Contains(msg, "must be 1-"),
		strings.Contains(msg, "nothing to update"):
		return http.StatusBadRequest
	case strings.Contains(msg, "not logged in"):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}
