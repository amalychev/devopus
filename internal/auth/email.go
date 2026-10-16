package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	mailer "github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/store"
)

// Plans an account can have. Only Free can be chosen now.
var plans = []*authv1.PlanInfo{
	{Plan: authv1.Plan_PLAN_FREE, Name: "Free", Description: "Cloud workspace for a small team", Available: true},
	{Plan: authv1.Plan_PLAN_SELF_HOSTED, Name: "Self-hosted", Description: "The workspace server on your own server (coming)"},
	{Plan: authv1.Plan_PLAN_TEAM, Name: "Team", Description: "Paid plan for larger teams (coming)"},
}

func planName(p authv1.Plan) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "PLAN_"))
}

func planOf(name string) authv1.Plan {
	return authv1.Plan(authv1.Plan_value["PLAN_"+strings.ToUpper(name)])
}

func (s *Service) ListPlans(context.Context, *connect.Request[authv1.ListPlansRequest]) (*connect.Response[authv1.ListPlansResponse], error) {
	return connect.NewResponse(&authv1.ListPlansResponse{Plans: plans}), nil
}

// normalizeEmail checks an email and lowercases it.
func normalizeEmail(e string) (string, error) {
	e = strings.TrimSpace(e)
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e[strings.LastIndex(e, "@")+1:], ".") {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is not an email address", e))
	}
	return strings.ToLower(e), nil
}

func (s *Service) StartEmailSignIn(ctx context.Context, req *connect.Request[authv1.StartEmailSignInRequest]) (*connect.Response[authv1.StartEmailSignInResponse], error) {
	m := req.Msg
	if m.Purpose != authv1.Purpose_PURPOSE_REGISTER && m.Purpose != authv1.Purpose_PURPOSE_SIGN_IN {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("purpose is not set"))
	}
	id, exp, err := s.startEmail(ctx, m.Email, m.Purpose, s.clientIP(req.Peer().Addr, req.Header()), m.DeviceId)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.StartEmailSignInResponse{ChallengeId: id, ExpiresAt: timestamppb.New(exp)}), nil
}

// startEmail sends a code to an email, within the limits per email, IP and device.
func (s *Service) startEmail(ctx context.Context, email string, purpose authv1.Purpose, ip, device string) (string, time.Time, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	since := now.Add(-time.Hour)
	for _, c := range []struct {
		count func() (int64, error)
		limit int64
	}{
		{func() (int64, error) {
			return s.q.CountChallengesByEmail(ctx, store.CountChallengesByEmailParams{Email: email, CreatedAt: since})
		}, LimitPerEmail},
		{func() (int64, error) {
			return s.q.CountChallengesByIP(ctx, store.CountChallengesByIPParams{Ip: ip, CreatedAt: since})
		}, LimitPerIP},
		{func() (int64, error) {
			if device == "" {
				return 0, nil
			}
			return s.q.CountChallengesByDevice(ctx, store.CountChallengesByDeviceParams{DeviceID: device, CreatedAt: since})
		}, LimitPerDevice},
	} {
		n, err := c.count()
		if err != nil {
			return "", time.Time{}, internal(err)
		}
		if n >= c.limit {
			return "", time.Time{}, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("too many codes requested; try again in an hour"))
		}
	}

	id, code := newID("chl"), otp()
	exp := now.Add(CodeTTL)
	if err := s.q.CreateChallenge(ctx, store.CreateChallengeParams{
		ID: id, Email: email, Purpose: purposeName(purpose), CodeHash: hash(id, code),
		Ip: ip, DeviceID: device, CreatedAt: now, ExpiresAt: exp,
	}); err != nil {
		return "", time.Time{}, internal(err)
	}
	what := "sign in to"
	if purpose == authv1.Purpose_PURPOSE_REGISTER {
		what = "create your account on"
	}
	err = s.cfg.Mailer.Send(ctx, mailer.Message{
		To:      email,
		Subject: fmt.Sprintf("Devopus code: %s", code),
		Text: fmt.Sprintf("Your code to %s Devopus: %s\n\nIt works for %d minutes. If you did not ask for it, ignore this email.\n",
			what, code, int(CodeTTL.Minutes())),
	})
	if err != nil {
		s.cfg.Log.Error("send code", "to", email, "err", err)
		return "", time.Time{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("could not send the email; try again later"))
	}
	return id, exp, nil
}

func purposeName(p authv1.Purpose) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "PURPOSE_"))
}

// verifyCode checks a code and spends its challenge. A wrong code counts as an
// attempt; after CodeAttempts the challenge is dead.
func (s *Service) verifyCode(ctx context.Context, challengeID, code string) (store.EmailChallenge, error) {
	var ch store.EmailChallenge
	wrong := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("the code is wrong or expired"))
	err := s.tx(ctx, func(q *store.Queries) error {
		var err error
		ch, err = q.ChallengeForUpdate(ctx, challengeID)
		if noRows(err) {
			return wrong
		}
		if err != nil {
			return internal(err)
		}
		now := s.now()
		switch {
		case ch.ConsumedAt != nil:
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the code was already used; ask for a new one"))
		case !now.Before(ch.ExpiresAt):
			return connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("the code expired; ask for a new one"))
		case ch.Attempts >= CodeAttempts:
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("too many wrong codes; ask for a new one"))
		}
		code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
		if subtle.ConstantTimeCompare(hash(ch.ID, code), ch.CodeHash) != 1 {
			if err := q.AddChallengeAttempt(ctx, ch.ID); err != nil {
				return internal(err)
			}
			if ch.Attempts+1 >= CodeAttempts {
				return commit(connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("too many wrong codes; ask for a new one")))
			}
			return commit(wrong)
		}
		return q.ConsumeChallenge(ctx, store.ConsumeChallengeParams{ID: ch.ID, ConsumedAt: &now})
	})
	return ch, err
}

func (s *Service) CompleteEmailSignIn(ctx context.Context, req *connect.Request[authv1.CompleteEmailSignInRequest]) (*connect.Response[authv1.CompleteEmailSignInResponse], error) {
	m := req.Msg
	ip := s.clientIP(req.Peer().Addr, req.Header())
	ch, err := s.verifyCode(ctx, m.ChallengeId, m.Code)
	if err != nil {
		return nil, err
	}
	var (
		user    store.User
		created bool
		tokens  *authv1.Tokens
	)
	err = s.tx(ctx, func(q *store.Queries) error {
		user, err = q.UserByEmail(ctx, ch.Email)
		switch {
		case noRows(err) && ch.Purpose == purposeName(authv1.Purpose_PURPOSE_REGISTER):
			if !available(m.Plan) {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("choose an available plan: free"))
			}
			user, err = q.CreateUser(ctx, store.CreateUserParams{ID: newID("usr"), Email: ch.Email, Plan: planName(m.Plan), CreatedAt: s.now()})
			if err != nil {
				return internal(err)
			}
			created = true
			s.event(ctx, q, user.ID, "", "registered", ip, "")
			s.event(ctx, q, user.ID, "", "plan_chosen", ip, user.Plan)
		case noRows(err):
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("there is no Devopus account for %s; create one with 'devopus register'", ch.Email))
		case err != nil:
			return internal(err)
		}
		tokens, err = s.openSession(ctx, q, user, m.DeviceName, ip, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.CompleteEmailSignInResponse{User: userInfo(user), Tokens: tokens, Created: created}), nil
}

func available(p authv1.Plan) bool {
	for _, pl := range plans {
		if pl.Plan == p {
			return pl.Available
		}
	}
	return false
}

// openSession starts a session for a user and issues its first tokens.
func (s *Service) openSession(ctx context.Context, q *store.Queries, user store.User, device, ip, audience string) (*authv1.Tokens, error) {
	now := s.now()
	device = strings.TrimSpace(device)
	if device == "" {
		device = "unknown device"
	}
	if len(device) > 100 {
		device = device[:100]
	}
	ses, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID: newID("ses"), UserID: user.ID, DeviceName: device, Ip: ip,
		CreatedAt: now, IdleExpiresAt: now.Add(SessionIdle), ExpiresAt: now.Add(SessionMax),
	})
	if err != nil {
		return nil, internal(err)
	}
	s.event(ctx, q, user.ID, ses.ID, "signed_in", ip, device)
	return s.issue(ctx, q, ses, audience)
}

// issue makes a new refresh token for a session and an access token.
func (s *Service) issue(ctx context.Context, q *store.Queries, ses store.Session, audience string) (*authv1.Tokens, error) {
	refresh := secret("dvr_")
	if err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{TokenHash: hash(refresh), SessionID: ses.ID, CreatedAt: s.now()}); err != nil {
		return nil, internal(err)
	}
	access, exp, err := s.signAccess(ses.UserID, ses.ID, audience)
	if err != nil {
		return nil, internal(err)
	}
	return &authv1.Tokens{
		AccessToken: access, AccessExpiresAt: timestamppb.New(exp),
		RefreshToken: refresh, Session: sessionInfo(ses, ses.ID),
	}, nil
}

// event writes to the account's security log; a failure is logged, not returned.
func (s *Service) event(ctx context.Context, q *store.Queries, userID, sessionID, kind, ip, detail string) {
	var sid *string
	if sessionID != "" {
		sid = &sessionID
	}
	if err := q.AddEvent(ctx, store.AddEventParams{UserID: userID, SessionID: sid, Kind: kind, Ip: ip, Detail: detail, CreatedAt: s.now()}); err != nil {
		s.cfg.Log.Error("security event", "kind", kind, "err", err)
	}
}

func userInfo(u store.User) *authv1.User {
	return &authv1.User{Id: u.ID, Email: u.Email, Plan: planOf(u.Plan), CreatedAt: timestamppb.New(u.CreatedAt)}
}

func sessionInfo(s store.Session, current string) *authv1.Session {
	return &authv1.Session{
		Id: s.ID, DeviceName: s.DeviceName,
		CreatedAt: timestamppb.New(s.CreatedAt), LastUsedAt: timestamppb.New(s.LastUsedAt),
		IdleExpiresAt: timestamppb.New(s.IdleExpiresAt), ExpiresAt: timestamppb.New(s.ExpiresAt),
		Current: s.ID == current,
	}
}
