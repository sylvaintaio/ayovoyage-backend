package identity

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const tagMFA = "Double authentification"

// staffFromRequest : compte visé par une étape d'activation — jeton d'étape
// (connexion) ou session du personnel déjà connecté.
func (h *Handler) staffFromRequest(ctx context.Context, token, step string) (uuid.UUID, error) {
	if token != "" {
		var id uuid.UUID
		err := h.S.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
			st, err := h.S.staffFromChallenge(ctx, q, token, step)
			id = st.ID
			return err
		})
		return id, err
	}
	if p, ok := auth.FromContext(ctx); ok && p.IsStaff() {
		return p.ID, nil
	}
	return uuid.Nil, errMFAStep
}

func (h *Handler) registerMFA(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("staff-mfa-verify", http.MethodPost, "/v1/auth/staff/mfa/verify", tagMFA,
		"Connexion du personnel (étape 2 : code)", apix.Public),
		"Code de l'application d'authentification, ou un code de secours à usage unique. Un code déjà utilisé est refusé."),
		func(ctx context.Context, in *struct {
			ClientType string `header:"X-Client-Type" enum:"web,mobile" default:"mobile"`
			Body       struct {
				MFAToken     string `json:"mfa_token"`
				Code         string `json:"code,omitempty" pattern:"^[0-9 ]{6,7}$" doc:"Code à 6 chiffres de l'application"`
				RecoveryCode string `json:"recovery_code,omitempty" maxLength:"20" doc:"Code de secours (à défaut du code)"`
			}
		}) (*SessionOutput, error) {
			if in.Body.Code == "" && in.Body.RecoveryCode == "" {
				return nil, huma.Error422UnprocessableEntity("Saisissez le code de votre application")
			}
			sess, err := h.S.VerifyMFA(ctx, in.Body.MFAToken, in.Body.Code, in.Body.RecoveryCode)
			if err != nil {
				return nil, apix.Err(err)
			}
			return h.sessionOutput(sess, in.ClientType, false), nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("staff-mfa-setup", http.MethodPost, "/v1/auth/staff/mfa/setup", tagMFA,
		"Activer la double authentification : obtenir le QR code", apix.Public),
		"Avec le jeton d'étape `setup` (connexion) ou une session du personnel. Le secret n'est actif qu'après `/activate`."),
		func(ctx context.Context, in *struct {
			Body struct {
				MFAToken string `json:"mfa_token,omitempty"`
			}
		}) (*struct{ Body MFASetup }, error) {
			id, err := h.staffFromRequest(ctx, in.Body.MFAToken, "setup")
			if err != nil {
				return nil, apix.Err(err)
			}
			out, err := h.S.SetupMFA(ctx, id)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body MFASetup }{Body: out}, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("staff-mfa-activate", http.MethodPost, "/v1/auth/staff/mfa/activate", tagMFA,
		"Activer la double authentification : confirmer avec un premier code", apix.Public),
		"Renvoie une session et **10 codes de secours** (affichés une seule fois)."),
		func(ctx context.Context, in *struct {
			ClientType    string `header:"X-Client-Type" enum:"web,mobile" default:"mobile"`
			RefreshCookie string `cookie:"bv_refresh"`
			Body          struct {
				MFAToken     string `json:"mfa_token,omitempty"`
				Code         string `json:"code" pattern:"^[0-9 ]{6,7}$"`
				RefreshToken string `json:"refresh_token,omitempty"`
			}
		}) (*SessionOutput, error) {
			id, err := h.staffFromRequest(ctx, in.Body.MFAToken, "setup")
			if err != nil {
				return nil, apix.Err(err)
			}
			family := uuid.Nil
			if in.Body.MFAToken == "" { // déjà connecté : la session actuelle est remplacée
				family = h.S.retireRefresh(ctx, id, firstNonEmpty(in.Body.RefreshToken, in.RefreshCookie))
			}
			sess, err := h.S.ActivateMFA(ctx, id, in.Body.Code, family)
			if err != nil {
				return nil, apix.Err(err)
			}
			return h.sessionOutput(sess, in.ClientType, false), nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("staff-mfa-step-up", http.MethodPost, "/v1/auth/staff/mfa/confirm", tagMFA,
		"Confirmer son identité pour une action sensible", apix.Staff),
		"À appeler quand une opération répond 403 avec l'en-tête `X-MFA-Required: step-up`, puis rejouer l'opération. "+
			"La vérification reste valable 15 minutes."),
		func(ctx context.Context, in *struct {
			ClientType    string `header:"X-Client-Type" enum:"web,mobile" default:"mobile"`
			RefreshCookie string `cookie:"bv_refresh"`
			Body          struct {
				Code         string `json:"code,omitempty" pattern:"^[0-9 ]{6,7}$" doc:"Code à 6 chiffres de l'application"`
				RecoveryCode string `json:"recovery_code,omitempty" maxLength:"20" doc:"Code de secours (à défaut du code)"`
				RefreshToken string `json:"refresh_token,omitempty"`
			}
		}) (*SessionOutput, error) {
			if in.Body.Code == "" && in.Body.RecoveryCode == "" {
				return nil, huma.Error422UnprocessableEntity("Saisissez le code de votre application")
			}
			sess, err := h.S.StepUp(ctx, auth.MustFromContext(ctx), firstNonEmpty(in.Body.RefreshToken, in.RefreshCookie),
				in.Body.Code, in.Body.RecoveryCode)
			if err != nil {
				return nil, apix.Err(err)
			}
			return h.sessionOutput(sess, in.ClientType, false), nil
		})

	huma.Register(api, apix.Op("get-my-mfa", http.MethodGet, "/v1/me/mfa", tagMFA, "État de ma double authentification", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body MFAStatus }, error) {
			st, err := h.S.MFAStatusOf(ctx, auth.MustFromContext(ctx).ID)
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body MFAStatus }{Body: st}, nil
		})

	huma.Register(api, apix.RequireFreshMFA(apix.Op("regenerate-my-recovery-codes", http.MethodPost, "/v1/me/mfa/recovery-codes", tagMFA,
		"Générer de nouveaux codes de secours", apix.Staff)),
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				RecoveryCodes []string `json:"recovery_codes"`
			}
		}, error) {
			codes, err := h.S.RegenerateRecoveryCodes(ctx, auth.MustFromContext(ctx).ID)
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct {
				Body struct {
					RecoveryCodes []string `json:"recovery_codes"`
				}
			}{}
			out.Body.RecoveryCodes = codes
			return out, nil
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("reset-staff-mfa", http.MethodPost, "/v1/admin/staff/{id}/mfa/reset", tagMFA,
		"Réinitialiser la double authentification d'un membre du personnel", apix.Staff, auth.RoleCompanyAdmin)),
		"Téléphone perdu : le secret et les codes de secours sont effacés, les sessions révoquées, et la personne "+
			"devra reconfigurer la double authentification à sa prochaine connexion. Impossible sur son propre compte."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{}, error) {
			return nil, apix.Err(h.S.ResetStaffMFA(ctx, auth.MustFromContext(ctx), in.ID))
		})
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// retireRefresh remplace le jeton de rafraîchissement actuel et renvoie sa famille.
func (s *Service) retireRefresh(ctx context.Context, staffID uuid.UUID, token string) uuid.UUID {
	family := uuid.Nil
	if token == "" {
		return family
	}
	_ = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		rt, err := q.GetRefreshTokenByHash(ctx, auth.HashToken(token))
		if err != nil || rt.SubjectID != staffID || rt.RevokedAt != nil {
			return nil
		}
		family = rt.FamilyID
		return q.RotateRefreshToken(ctx, store.RotateRefreshTokenParams{ID: rt.ID})
	})
	return family
}
