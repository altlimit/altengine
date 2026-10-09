package identity

import (
	"log"
	"net/http"
	"time"

	"github.com/altlimit/altengine/cli/internal/auth"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Generalized data-plane authentication. A /v1 bearer credential is EITHER an org API key
// (a full-trust backend credential) or an end-user identity token minted by an auth
// instance (a JWT — three dot-parts). Resolving the identity token means:
//
//  1. peek the unverified `iss` (the issuing auth instance id) and load that instance,
//  2. verify the signature (and expiry) against that instance's secret,
//  3. derive grants from the instance's `access` config — each "service:instance" entry's
//     level plugs straight into the existing grants map, so grant checks are unchanged,
//  4. carry the verified identity along so the row-rule engine can substitute `$auth.*`.
//
// Cross-tenant isolation is structural in the hosted service (the token can only ever
// address instances in its own org). The emulator is single-tenant, so there is only the
// local dev org — but the shape is kept honest: only targets listed in `access` are
// granted, everything else is default-deny.

// EndUser is a verified end-user identity behind an identity-token request.
type EndUser struct {
	AuthInstanceID string
	AuthInstance   string // the auth instance NAME (its path segment)
	UID            string
	Identifier     string
	Email          string
	HasEmail       bool
	// The two identity bags — the split is a security boundary. Profile is user-supplied
	// (signup), read as `$auth.profile.X`, NEVER authoritative. Claims is server/admin-set,
	// read as `$auth.claims.X`, safe to authorize on.
	Profile map[string]any
	Claims  map[string]any
	// The identity token's expiry (unix seconds) and credential epoch — what a credential
	// derived from it (a channel subscriber token) must not outlive, and is revoked with.
	Exp int64
	Ce  int64
	// The issuing instance's `access` config, carried so the rule engine reads the
	// per-target rules without a second lookup.
	Access AccessConfig
}

// Grants derives the API-key-style grants map from the identity's access config. Keys are
// already in the "service:instance" form grant checks expect.
func (u *EndUser) Grants() auth.Grants {
	g := auth.Grants{}
	for target, entry := range u.Access {
		g[target] = entry.Level
	}
	return g
}

// Service resolves identity tokens and owns the end-user stores. It is shared by the auth
// data plane (which mints tokens) and by the datastore/channel data planes (which accept
// them).
type Service struct {
	Reg     *control.Registry
	Mgr     *Manager
	DevOpen bool // local-dev conveniences (echoing one-time codes in responses)
}

// NewService builds the identity service.
func NewService(reg *control.Registry, mgr *Manager, devOpen bool) *Service {
	// Turning the email-verification gate on grandfathers the accounts that predate it, as
	// hosted — only on the off→on transition, and never an account that signed up gated.
	reg.OnConfigSaved("auth", func(in *control.Instance, before map[string]any) {
		if !ParseConfig(in.Config()).RequireEmailVerification || ParseConfig(before).RequireEmailVerification {
			return
		}
		store, err := mgr.Open(in.ID)
		if err != nil {
			log.Printf("[auth] grandfathering %s: %v", in.Name, err)
			return
		}
		if _, err := store.GrandfatherVerified(time.Now().UnixMilli()); err != nil {
			log.Printf("[auth] grandfathering %s: %v", in.Name, err)
		}
	})
	return &Service{Reg: reg, Mgr: mgr, DevOpen: devOpen}
}

// LooksLikeJWT reports whether a bearer credential is an identity token rather than an
// API key.
func LooksLikeJWT(token string) bool { return common.LooksLikeJWT(token) }

// ResolveToken verifies an identity token and returns the end user it authenticates.
func (s *Service) ResolveToken(token string) (*EndUser, error) {
	iss := PeekIssuer(token)
	if iss == "" {
		return nil, common.Unauthenticated("invalid token")
	}
	inst := s.Reg.GetByID("auth", iss)
	if inst == nil {
		return nil, common.Unauthenticated("invalid token")
	}
	claims := VerifyIdentity(token, inst.Secret)
	if claims == nil || claims.Iss != inst.ID {
		return nil, common.Unauthenticated("invalid or expired token")
	}
	// A valid signature is not a live account: one disabled, deleted or reset since this
	// token was minted is refused here rather than for the rest of the token's TTL.
	store, err := s.Mgr.Open(inst.ID)
	if err != nil {
		return nil, err
	}
	if dead, err := revoked(store, claims); err != nil {
		return nil, err
	} else if dead {
		return nil, common.Unauthenticated("invalid or expired token")
	}
	cfg := ParseConfig(inst.Config())
	p := claims.Profile
	if p == nil {
		p = map[string]any{}
	}
	c := claims.Claims
	if c == nil {
		c = map[string]any{}
	}
	return &EndUser{
		AuthInstanceID: inst.ID,
		AuthInstance:   inst.Name,
		UID:            claims.Sub,
		Identifier:     claims.Identifier,
		Email:          claims.Email,
		HasEmail:       claims.Email != "",
		Profile:        p,
		Claims:         c,
		Exp:            claims.Exp,
		Ce:             claims.Ce,
		Access:         cfg.Access,
	}, nil
}

// IsRevoked reports whether a credential derived from an identity token — minted for uid by
// the auth instance authInstanceID, in credential epoch ce — has since been revoked. A
// channel subscriber token minted for an end user is the case it exists for.
func (s *Service) IsRevoked(authInstanceID, uid string, ce int64) (bool, error) {
	if s.Reg.GetByID("auth", authInstanceID) == nil {
		return true, nil
	}
	store, err := s.Mgr.Open(authInstanceID)
	if err != nil {
		return false, err
	}
	return revoked(store, &IdentityClaims{Sub: uid, Ce: ce})
}

// revoked reports whether verified identity claims were revoked since they were minted: the
// account was disabled, deleted or reset, moving it to a later credential epoch.
func revoked(store *Store, c *IdentityClaims) (bool, error) {
	epoch, ok, err := store.RevokedEpoch(c.Sub, time.Now().UnixMilli())
	if err != nil || !ok {
		return false, err
	}
	return c.Ce < epoch, nil
}

// ResolveRequest resolves a data-plane request to a caller. When the bearer credential is
// an identity token the returned EndUser is non-nil and the auth.Identity carries the
// grants derived from its access config; otherwise the API-key path is taken and the
// EndUser is nil.
//
// A nil *Service (no auth service wired) simply falls through to the API-key path.
func (s *Service) ResolveRequest(r *http.Request, keys *auth.Store) (*auth.Identity, *EndUser, error) {
	tok := auth.Bearer(r)
	if s != nil && tok != "" && LooksLikeJWT(tok) {
		u, err := s.ResolveToken(tok)
		if err != nil {
			return nil, nil, err
		}
		return &auth.Identity{OrgID: auth.DevOrgID, Grants: u.Grants()}, u, nil
	}
	id, err := keys.Resolve(r)
	if err != nil {
		return nil, nil, err
	}
	return id, nil, nil
}
