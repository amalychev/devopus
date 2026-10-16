package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/internal/auth/store"
)

// authorizeRequest is the browser sign-in request the CLI opens (RFC 6749 §4.1.1 with
// PKCE, RFC 7636).
type authorizeRequest struct {
	ClientID, RedirectURI, State, CodeChallenge, Method, ResponseType string
}

func authorizeRequestFrom(v url.Values) authorizeRequest {
	return authorizeRequest{
		ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state"),
		CodeChallenge: v.Get("code_challenge"), Method: v.Get("code_challenge_method"), ResponseType: v.Get("response_type"),
	}
}

// check validates the request. The redirect goes only to a loopback address
// (RFC 8252 §7.3): the CLI listens on 127.0.0.1 with a random port.
func (r authorizeRequest) check() error {
	switch {
	case r.ClientID != ClientID:
		return fmt.Errorf("unknown client %q", r.ClientID)
	case r.ResponseType != "code":
		return errors.New("response_type must be code")
	case r.Method != "S256":
		return errors.New("code_challenge_method must be S256")
	case len(r.CodeChallenge) != 43:
		return errors.New("code_challenge is not a SHA-256 PKCE challenge")
	case r.State == "":
		return errors.New("state is missing")
	}
	return checkRedirect(r.RedirectURI)
}

func checkRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Port() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("redirect_uri %q must be http://127.0.0.1:<port>/…", raw)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("redirect_uri %q must point to 127.0.0.1", raw)
	}
	return nil
}

// authorizationCode issues the code that comes back to the CLI's callback.
func (s *Service) authorizationCode(ctx context.Context, user store.User, r authorizeRequest) (string, error) {
	code := secret("dva_")
	now := s.now()
	err := s.q.CreateAuthorizationCode(ctx, store.CreateAuthorizationCodeParams{
		CodeHash: hash(code), UserID: user.ID, CodeChallenge: r.CodeChallenge, RedirectUri: r.RedirectURI,
		CreatedAt: now, ExpiresAt: now.Add(AuthCodeTTL),
	})
	return code, err
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Service) ExchangeAuthorizationCode(ctx context.Context, req *connect.Request[authv1.ExchangeAuthorizationCodeRequest]) (*connect.Response[authv1.ExchangeAuthorizationCodeResponse], error) {
	m := req.Msg
	ip := s.clientIP(req.Peer().Addr, req.Header())
	var (
		user   store.User
		tokens *authv1.Tokens
	)
	bad := connect.NewError(connect.CodeInvalidArgument, errors.New("the sign-in code is wrong, used or expired; run 'devopus login' again"))
	err := s.tx(ctx, func(q *store.Queries) error {
		ac, err := q.AuthorizationCodeForUpdate(ctx, hash(m.Code))
		if noRows(err) {
			return bad
		}
		if err != nil {
			return internal(err)
		}
		now := s.now()
		if ac.UsedAt != nil || !now.Before(ac.ExpiresAt) || ac.RedirectUri != m.RedirectUri ||
			subtle.ConstantTimeCompare([]byte(pkceChallenge(m.CodeVerifier)), []byte(ac.CodeChallenge)) != 1 {
			return bad
		}
		if err := q.UseAuthorizationCode(ctx, store.UseAuthorizationCodeParams{CodeHash: ac.CodeHash, UsedAt: &now}); err != nil {
			return internal(err)
		}
		if user, err = q.UserByID(ctx, ac.UserID); err != nil {
			return internal(err)
		}
		tokens, err = s.openSession(ctx, q, user, m.DeviceName, ip, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&authv1.ExchangeAuthorizationCodeResponse{User: userInfo(user), Tokens: tokens}), nil
}

func (s *Service) StartDeviceSignIn(ctx context.Context, req *connect.Request[authv1.StartDeviceSignInRequest]) (*connect.Response[authv1.StartDeviceSignInResponse], error) {
	ip := s.clientIP(req.Peer().Addr, req.Header())
	device := secret("dvd_")
	now := s.now()
	exp := now.Add(DeviceCodeTTL)
	var uc string
	for range 5 {
		uc = userCode()
		err := s.q.CreateDeviceCode(ctx, store.CreateDeviceCodeParams{
			DeviceCodeHash: hash(device), UserCode: uc, DeviceName: req.Msg.DeviceName, Ip: ip, CreatedAt: now, ExpiresAt: exp,
		})
		if err == nil {
			page := s.cfg.Issuer + "/device"
			return connect.NewResponse(&authv1.StartDeviceSignInResponse{
				DeviceCode: device, UserCode: uc, VerificationUri: page,
				VerificationUriComplete: page + "?user_code=" + url.QueryEscape(uc),
				ExpiresAt:               timestamppb.New(exp), IntervalSeconds: int32(DeviceInterval / time.Second),
			}), nil
		}
		s.cfg.Log.Warn("device code", "err", err) // a user code collision: try another
	}
	return nil, internal(errors.New("could not make a device code"))
}

func (s *Service) PollDeviceSignIn(ctx context.Context, req *connect.Request[authv1.PollDeviceSignInRequest]) (*connect.Response[authv1.PollDeviceSignInResponse], error) {
	resp := &authv1.PollDeviceSignInResponse{}
	err := s.tx(ctx, func(q *store.Queries) error {
		dc, err := q.DeviceCodeForUpdate(ctx, hash(req.Msg.DeviceCode))
		if noRows(err) {
			return connect.NewError(connect.CodeNotFound, errors.New("unknown device code"))
		}
		if err != nil {
			return internal(err)
		}
		now := s.now()
		switch {
		case dc.ConsumedAt != nil:
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("this device code was already used"))
		case dc.ApprovedAt != nil:
			if err := q.ConsumeDeviceCode(ctx, store.ConsumeDeviceCodeParams{DeviceCodeHash: dc.DeviceCodeHash, ConsumedAt: &now}); err != nil {
				return internal(err)
			}
			user, err := q.UserByID(ctx, *dc.UserID)
			if err != nil {
				return internal(err)
			}
			resp.Tokens, err = s.openSession(ctx, q, user, dc.DeviceName, dc.Ip, "")
			resp.User, resp.Status = userInfo(user), authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_DONE
			return err
		case !now.Before(dc.ExpiresAt):
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the code expired; run 'devopus login --device' again"))
		}
		resp.Status = authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_PENDING
		// A poll sooner than the interval (with a second of slack) asks the device to slow down.
		if dc.LastPolledAt != nil && now.Sub(*dc.LastPolledAt) < DeviceInterval-time.Second {
			resp.Status = authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_SLOW_DOWN
		}
		return q.PollDeviceCode(ctx, store.PollDeviceCodeParams{DeviceCodeHash: dc.DeviceCodeHash, LastPolledAt: &now})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// pendingDevice finds a device sign-in waiting for its user code.
func (s *Service) pendingDevice(ctx context.Context, userCode string) (store.DeviceCode, error) {
	dc, err := s.q.DeviceCodeByUserCode(ctx, normalizeUserCode(userCode))
	if noRows(err) || (err == nil && (dc.ApprovedAt != nil || !s.now().Before(dc.ExpiresAt))) {
		return dc, errors.New("this code is wrong or expired; check the terminal for the current one")
	}
	return dc, err
}

// approveDevice confirms a device sign-in for a user.
func (s *Service) approveDevice(ctx context.Context, userCode string, user store.User) error {
	now := s.now()
	n, err := s.q.ApproveDeviceCode(ctx, store.ApproveDeviceCodeParams{UserCode: normalizeUserCode(userCode), UserID: &user.ID, ApprovedAt: &now})
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("this code is wrong or expired; check the terminal for the current one")
	}
	return nil
}
