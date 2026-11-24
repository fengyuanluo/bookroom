package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/auth"
)

func toAPIUser(u auth.User) api.User {
	out := api.User{Id: u.ID, Username: u.Username, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt}
	if u.Email != "" {
		out.Email = &u.Email
	}
	return out
}

func toTokenPair(t auth.Tokens) api.TokenPair {
	return api.TokenPair{AccessToken: t.Access, RefreshToken: t.Refresh, TokenType: "Bearer", ExpiresIn: t.ExpiresIn,
		ExpiresAt: t.ExpiresAt, User: toAPIUser(t.User)}
}

func (s *Server) GetHealth(ctx context.Context, _ api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	if err := s.Store.R.PingContext(rc(ctx)); err != nil {
		return nil, apperr.New(http.StatusServiceUnavailable, "unavailable", "database unavailable")
	}
	return api.GetHealth200JSONResponse{Status: "ok", Version: s.Version}, nil
}

func (s *Server) GetOpenAPI(context.Context, api.GetOpenAPIRequestObject) (api.GetOpenAPIResponseObject, error) {
	b, err := s.spec.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return api.GetOpenAPI200JSONResponse(m), nil
}

func (s *Server) Login(ctx context.Context, r api.LoginRequestObject) (api.LoginResponseObject, error) {
	t, err := s.Auth.Login(rc(ctx), r.Body.Username, r.Body.Password)
	if err != nil {
		return nil, err
	}
	return api.Login200JSONResponse(toTokenPair(t)), nil
}

func (s *Server) RefreshToken(ctx context.Context, r api.RefreshTokenRequestObject) (api.RefreshTokenResponseObject, error) {
	t, err := s.Auth.Refresh(rc(ctx), r.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	return api.RefreshToken200JSONResponse(toTokenPair(t)), nil
}

func (s *Server) Logout(ctx context.Context, r api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	if err := s.Auth.Logout(rc(ctx), r.Body.RefreshToken); err != nil {
		return nil, err
	}
	return api.Logout204Response{}, nil
}

func (s *Server) GetMe(ctx context.Context, _ api.GetMeRequestObject) (api.GetMeResponseObject, error) {
	return api.GetMe200JSONResponse(toAPIUser(currentUser(ctx))), nil
}

func (s *Server) ChangePassword(ctx context.Context, r api.ChangePasswordRequestObject) (api.ChangePasswordResponseObject, error) {
	if err := s.Auth.ChangePassword(rc(ctx), currentUser(ctx).ID, r.Body.CurrentPassword, r.Body.NewPassword); err != nil {
		return nil, err
	}
	return api.ChangePassword204Response{}, nil
}

func (s *Server) ListUsers(ctx context.Context, _ api.ListUsersRequestObject) (api.ListUsersResponseObject, error) {
	users, err := s.Auth.ListUsers(rc(ctx))
	if err != nil {
		return nil, err
	}
	out := api.UserList{Items: make([]api.User, len(users))}
	for i, u := range users {
		out.Items[i] = toAPIUser(u)
	}
	return api.ListUsers200JSONResponse(out), nil
}

func (s *Server) CreateUser(ctx context.Context, r api.CreateUserRequestObject) (api.CreateUserResponseObject, error) {
	email := ""
	if r.Body.Email != nil {
		email = string(*r.Body.Email)
	}
	u, err := s.Auth.CreateUser(rc(ctx), r.Body.Username, r.Body.Password, email, r.Body.IsAdmin != nil && *r.Body.IsAdmin)
	if err != nil {
		return nil, err
	}
	return api.CreateUser201JSONResponse(toAPIUser(u)), nil
}

func (s *Server) DeleteUser(ctx context.Context, r api.DeleteUserRequestObject) (api.DeleteUserResponseObject, error) {
	if err := s.Auth.DeleteUser(rc(ctx), currentUser(ctx).ID, r.UserId); err != nil {
		return nil, err
	}
	if err := s.Storage.PurgeUser(r.UserId); err != nil {
		s.Log.Warn("purge user objects", "user", r.UserId, "err", err)
	}
	return api.DeleteUser204Response{}, nil
}

func (s *Server) ResetUserPassword(ctx context.Context, r api.ResetUserPasswordRequestObject) (api.ResetUserPasswordResponseObject, error) {
	if err := s.Auth.SetPassword(rc(ctx), r.UserId, r.Body.Password); err != nil {
		return nil, err
	}
	return api.ResetUserPassword204Response{}, nil
}

func toAPIDevice(d auth.Device) api.Device {
	return api.Device{Id: d.ID, Name: d.Name, CreatedAt: d.CreatedAt, LastUsedAt: d.LastUsedAt}
}

func (s *Server) ListDevices(ctx context.Context, _ api.ListDevicesRequestObject) (api.ListDevicesResponseObject, error) {
	ds, err := s.Auth.ListDevices(rc(ctx), currentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	out := api.DeviceList{Items: make([]api.Device, len(ds))}
	for i, d := range ds {
		out.Items[i] = toAPIDevice(d)
	}
	return api.ListDevices200JSONResponse(out), nil
}

func (s *Server) CreateDevice(ctx context.Context, r api.CreateDeviceRequestObject) (api.CreateDeviceResponseObject, error) {
	u := currentUser(ctx)
	d, key, err := s.Auth.CreateDevice(rc(ctx), u.ID, r.Body.Name)
	if err != nil {
		return nil, err
	}
	return api.CreateDevice201JSONResponse{Device: toAPIDevice(d), Key: key, KosyncUsername: u.Username}, nil
}

func (s *Server) DeleteDevice(ctx context.Context, r api.DeleteDeviceRequestObject) (api.DeleteDeviceResponseObject, error) {
	if err := s.Auth.DeleteDevice(rc(ctx), currentUser(ctx).ID, r.DeviceId); err != nil {
		return nil, err
	}
	return api.DeleteDevice204Response{}, nil
}

// ── GoTrue subset (readest.koplugin login) ──

func goTrueUser(u auth.User) api.GoTrueUser {
	email := u.Email
	if email == "" {
		email = u.Username
	}
	return api.GoTrueUser{Id: u.ID, Aud: "authenticated", Role: "authenticated", Email: email,
		UserMetadata: &map[string]any{"username": u.Username}}
}

func (s *Server) GotrueToken(ctx context.Context, r api.GotrueTokenRequestObject) (api.GotrueTokenResponseObject, error) {
	var t auth.Tokens
	var err error
	switch r.Params.GrantType {
	case api.Password:
		if r.Body.Email == nil || r.Body.Password == nil {
			return nil, apperr.BadRequest("email and password are required")
		}
		t, err = s.Auth.Login(rc(ctx), *r.Body.Email, *r.Body.Password)
		if ae, ok := apperr.As(err); ok && ae.Status == http.StatusUnauthorized {
			err = apperr.BadRequest("Invalid login credentials")
		}
	case api.RefreshToken:
		if r.Body.RefreshToken == nil {
			return nil, apperr.BadRequest("refresh_token is required")
		}
		t, err = s.Auth.Refresh(rc(ctx), *r.Body.RefreshToken)
	default:
		return nil, apperr.BadRequest("unsupported grant_type")
	}
	if err != nil {
		return nil, err
	}
	return api.GotrueToken200JSONResponse{AccessToken: t.Access, TokenType: "bearer", ExpiresIn: t.ExpiresIn,
		ExpiresAt: t.ExpiresAt.Unix(), RefreshToken: t.Refresh, User: goTrueUser(t.User)}, nil
}

func (s *Server) GotrueLogout(ctx context.Context, _ api.GotrueLogoutRequestObject) (api.GotrueLogoutResponseObject, error) {
	if err := s.Auth.RevokeAll(rc(ctx), currentUser(ctx).ID); err != nil {
		return nil, err
	}
	return api.GotrueLogout204Response{}, nil
}

func (s *Server) GotrueUser(ctx context.Context, _ api.GotrueUserRequestObject) (api.GotrueUserResponseObject, error) {
	return api.GotrueUser200JSONResponse(goTrueUser(currentUser(ctx))), nil
}
