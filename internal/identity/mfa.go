package identity

import (
	"context"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Double authentification (TOTP) du personnel.
//
//   - Obligatoire pour tous les rôles du back-office ; les rôles de l'application
//     Agent (terminal enregistré et révocable) en sont exemptés.
//   - Comptes existants : période de grâce (mfa_required_from) ; ensuite, activation
//     imposée à la connexion.
//   - Actions sensibles : code redemandé si la dernière vérification date de plus
//     de 15 minutes (apix.RequireFreshMFA).

const (
	challengeTTL      = 5 * time.Minute
	recoveryCodeCount = 10
)

// MFAChallenge : étape intermédiaire après le mot de passe.
type MFAChallenge struct {
	Step      string    // verify : saisir le code ; setup : activer la double authentification
	Token     string    // jeton d'étape (5 minutes)
	ExpiresAt time.Time //
}

type MFASetup struct {
	Secret string `json:"secret" doc:"Clé à saisir à la main dans l'application si le QR code ne peut pas être scanné"`
	URI    string `json:"otpauth_uri" doc:"Contenu du QR code (otpauth://)"`
}

var (
	errMFACode     = huma.Error400BadRequest("Code incorrect. Vérifiez l'heure de votre téléphone et réessayez.")
	errMFAStep     = huma.Error401Unauthorized("Étape de connexion expirée, saisissez à nouveau votre mot de passe")
	errMFAEnabled  = huma.Error409Conflict("La double authentification est déjà activée sur ce compte")
	errMFANotSetup = huma.Error409Conflict("Commencez par scanner le QR code d'activation")
)

// mfaStep : étape exigée après le mot de passe ("" = session directe).
func (s *Service) mfaStep(st store.StaffUser) string {
	switch {
	case auth.MFAExempt(st.Role):
		return ""
	case st.TotpEnabledAt != nil:
		return "verify"
	case time.Now().Before(st.MfaRequiredFrom):
		return "" // période de grâce
	default:
		return "setup"
	}
}

func (s *Service) challenge(st store.StaffUser, step string) (*MFAChallenge, error) {
	tok, exp, err := s.Tokens.IssueChallenge(st.ID, step, challengeTTL)
	if err != nil {
		return nil, err
	}
	return &MFAChallenge{Step: step, Token: tok, ExpiresAt: exp}, nil
}

func (s *Service) box() (*auth.SecretBox, error) { return auth.NewSecretBox(s.Cfg.MFAEncryptionKey) }

// staffFromChallenge vérifie le jeton d'étape et recharge le compte.
func (s *Service) staffFromChallenge(ctx context.Context, q *store.Queries, token, wantStep string) (store.StaffUser, error) {
	id, step, err := s.Tokens.ParseChallenge(token)
	if err != nil || step != wantStep {
		return store.StaffUser{}, errMFAStep
	}
	st, err := q.GetStaff(ctx, id)
	if err != nil || !st.IsActive {
		return st, errMFAStep
	}
	if st.LockedUntil != nil && st.LockedUntil.After(time.Now()) {
		return st, errLocked
	}
	return st, nil
}

// checkCode vérifie un code TOTP ou un code de secours. Les échecs comptent
// dans le verrouillage du compte (5 essais), comme le mot de passe.
func (s *Service) checkCode(ctx context.Context, q *store.Queries, st store.StaffUser, code, recovery string) (usedRecovery bool, err error) {
	if recovery != "" {
		_, err := q.UseStaffRecoveryCode(ctx, store.UseStaffRecoveryCodeParams{StaffID: st.ID, CodeHash: auth.HashRecoveryCode(recovery)})
		if database.IsNotFound(err) {
			return false, errMFACode
		}
		return err == nil, err
	}
	if st.TotpSecretEnc == nil {
		return false, errMFANotSetup
	}
	box, err := s.box()
	if err != nil {
		return false, err
	}
	secret, err := box.Open(*st.TotpSecretEnc)
	if err != nil {
		return false, err
	}
	step, ok := auth.ValidateTOTP(secret, code, time.Now(), st.TotpLastStep)
	if !ok {
		return false, errMFACode
	}
	return false, q.SetStaffTOTPStep(ctx, store.SetStaffTOTPStepParams{ID: st.ID, LastStep: step})
}

// withFailureCount exécute fn ; une erreur de code incrémente le compteur
// d'échecs dans une transaction séparée (validée même si fn échoue).
func (s *Service) withFailureCount(ctx context.Context, staffID uuid.UUID, fn func(q *store.Queries) error) error {
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error { return fn(q) })
	if err == errMFACode {
		_ = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
			return q.RecordStaffLoginFailure(ctx, staffID)
		})
	}
	return err
}

func nowPtr() *time.Time {
	t := time.Now()
	return &t
}

// VerifyMFA : deuxième étape de la connexion (code de l'application ou code de secours).
func (s *Service) VerifyMFA(ctx context.Context, challenge, code, recovery string) (Session, error) {
	staffID, _, _ := s.Tokens.ParseChallenge(challenge)
	var (
		st           store.StaffUser
		usedRecovery bool
	)
	err := s.withFailureCount(ctx, staffID, func(q *store.Queries) error {
		var err error
		if st, err = s.staffFromChallenge(ctx, q, challenge, "verify"); err != nil {
			return err
		}
		if usedRecovery, err = s.checkCode(ctx, q, st, code, recovery); err != nil {
			return err
		}
		if err := q.RecordStaffLoginSuccess(ctx, st.ID); err != nil {
			return err
		}
		if usedRecovery {
			return s.audit(ctx, q, st, "staff.mfa_recovery_code_used", nil)
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if usedRecovery {
		s.alert(st, "Un code de secours a été utilisé",
			"Un code de secours vient d’être utilisé pour vous connecter. Si ce n’est pas vous, prévenez immédiatement votre administrateur.")
	}
	p := staffPrincipal(st)
	p.MFAAt = nowPtr()
	return s.openSession(ctx, p, uuid.Nil)
}

// SetupMFA génère un nouveau secret (non actif tant qu'il n'est pas confirmé).
// staffID provient soit du jeton d'étape « setup », soit de la session.
func (s *Service) SetupMFA(ctx context.Context, staffID uuid.UUID) (MFASetup, error) {
	var out MFASetup
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		st, err := q.GetStaff(ctx, staffID)
		if err != nil {
			return err
		}
		if st.TotpEnabledAt != nil {
			return errMFAEnabled
		}
		box, err := s.box()
		if err != nil {
			return err
		}
		secret := auth.NewTOTPSecret()
		out = MFASetup{Secret: secret, URI: auth.TOTPURI(s.Cfg.BrandName, st.Email, secret)}
		return q.SetStaffTOTPPending(ctx, store.SetStaffTOTPPendingParams{ID: st.ID, SecretEnc: ptr(box.Seal(secret))})
	})
	return out, err
}

// ActivateMFA confirme l'activation avec un premier code et génère les codes de secours.
func (s *Service) ActivateMFA(ctx context.Context, staffID uuid.UUID, code string, familyID uuid.UUID) (Session, error) {
	var (
		st    store.StaffUser
		codes []string
	)
	err := s.withFailureCount(ctx, staffID, func(q *store.Queries) error {
		var err error
		if st, err = q.GetStaff(ctx, staffID); err != nil {
			return err
		}
		if st.TotpEnabledAt != nil {
			return errMFAEnabled
		}
		if _, err := s.checkCode(ctx, q, st, code, ""); err != nil {
			return err
		}
		st, _ = q.GetStaff(ctx, staffID) // dernier pas utilisé mis à jour
		if err := q.EnableStaffTOTP(ctx, store.EnableStaffTOTPParams{ID: st.ID, LastStep: st.TotpLastStep}); err != nil {
			return err
		}
		if codes, err = s.newRecoveryCodes(ctx, q, st); err != nil {
			return err
		}
		if err := q.RecordStaffLoginSuccess(ctx, st.ID); err != nil {
			return err
		}
		return s.audit(ctx, q, st, "staff.mfa_enabled", nil)
	})
	if err != nil {
		return Session{}, err
	}
	s.alert(st, "Double authentification activée",
		"La double authentification est désormais active sur votre compte. Un code de votre application vous sera demandé à chaque connexion.")
	p := staffPrincipal(st)
	p.MFAAt = nowPtr()
	sess, err := s.openSession(ctx, p, familyID)
	sess.RecoveryCodes = codes
	return sess, err
}

func (s *Service) newRecoveryCodes(ctx context.Context, q *store.Queries, st store.StaffUser) ([]string, error) {
	if err := q.DeleteStaffRecoveryCodes(ctx, st.ID); err != nil {
		return nil, err
	}
	codes := auth.NewRecoveryCodes(recoveryCodeCount)
	for _, c := range codes {
		if err := q.InsertStaffRecoveryCode(ctx, store.InsertStaffRecoveryCodeParams{
			CompanyID: st.CompanyID, StaffID: st.ID, CodeHash: auth.HashRecoveryCode(c)}); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// RegenerateRecoveryCodes remplace tous les codes de secours (action sensible).
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, staffID uuid.UUID) ([]string, error) {
	var codes []string
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		st, err := q.GetStaff(ctx, staffID)
		if err != nil {
			return err
		}
		if st.TotpEnabledAt == nil {
			return errMFANotSetup
		}
		if codes, err = s.newRecoveryCodes(ctx, q, st); err != nil {
			return err
		}
		return s.audit(ctx, q, st, "staff.mfa_recovery_codes_regenerated", nil)
	})
	return codes, err
}

// StepUp : nouvelle saisie du code pour une action sensible. La session est
// renouvelée (même famille) avec l'heure de vérification.
func (s *Service) StepUp(ctx context.Context, p auth.Principal, refreshToken, code, recovery string) (Session, error) {
	var (
		st     store.StaffUser
		family = uuid.Nil
	)
	err := s.withFailureCount(ctx, p.ID, func(q *store.Queries) error {
		var err error
		if st, err = q.GetStaff(ctx, p.ID); err != nil {
			return err
		}
		if st.TotpEnabledAt == nil {
			return huma.Error409Conflict("Activez d'abord la double authentification dans « Sécurité du compte »")
		}
		if st.LockedUntil != nil && st.LockedUntil.After(time.Now()) {
			return errLocked
		}
		if _, err := s.checkCode(ctx, q, st, code, recovery); err != nil {
			return err
		}
		if refreshToken != "" { // l'ancien jeton de rafraîchissement est remplacé
			if rt, err := q.GetRefreshTokenByHash(ctx, auth.HashToken(refreshToken)); err == nil && rt.SubjectID == st.ID && rt.RevokedAt == nil {
				family = rt.FamilyID
				if err := q.RotateRefreshToken(ctx, store.RotateRefreshTokenParams{ID: rt.ID}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	np := staffPrincipal(st)
	np.MFAAt = nowPtr()
	return s.openSession(ctx, np, family)
}

// ResetStaffMFA : un administrateur réinitialise la double authentification
// d'un collègue (téléphone perdu). Sessions révoquées, nouvelle activation à la
// prochaine connexion. Impossible sur son propre compte.
func (s *Service) ResetStaffMFA(ctx context.Context, admin auth.Principal, staffID uuid.UUID) error {
	if admin.ID == staffID {
		return huma.Error403Forbidden("Vous ne pouvez pas réinitialiser votre propre double authentification : demandez à un autre administrateur")
	}
	var st store.StaffUser
	err := s.DB.Tx(ctx, admin.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		st, err = q.GetStaff(ctx, staffID)
		if database.IsNotFound(err) || (err == nil && st.CompanyID != admin.CompanyID) {
			return apix.NotFound("Membre du personnel")
		}
		if err != nil {
			return err
		}
		if err := q.ResetStaffTOTP(ctx, store.ResetStaffTOTPParams{ID: st.ID, CompanyID: admin.CompanyID}); err != nil {
			return err
		}
		if err := q.DeleteStaffRecoveryCodes(ctx, st.ID); err != nil {
			return err
		}
		if err := q.RevokeSubjectRefreshTokens(ctx, store.RevokeSubjectRefreshTokensParams{SubjectType: string(auth.SubjectStaff), SubjectID: st.ID}); err != nil {
			return err
		}
		return apix.Audit(ctx, q, &admin.CompanyID, "staff.mfa_reset", "staff", st.ID.String(), nil, map[string]any{"email": st.Email})
	})
	if err == nil {
		s.alert(st, "Double authentification réinitialisée",
			"Un administrateur a réinitialisé la double authentification de votre compte. À votre prochaine connexion, vous devrez la configurer à nouveau. Si vous n’êtes pas à l’origine de cette demande, prévenez immédiatement votre responsable.")
	}
	return err
}

// MFAStatus : état de la double authentification du compte connecté.
type MFAStatus struct {
	Required          bool       `json:"required" doc:"Faux pour les rôles de l'application Agent"`
	Enabled           bool       `json:"enabled"`
	EnabledAt         *time.Time `json:"enabled_at,omitempty"`
	RequiredFrom      *time.Time `json:"required_from,omitempty" doc:"Date limite d'activation (période de grâce)"`
	RecoveryCodesLeft int32      `json:"recovery_codes_left"`
}

func (s *Service) MFAStatusOf(ctx context.Context, staffID uuid.UUID) (MFAStatus, error) {
	var out MFAStatus
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		st, err := q.GetStaff(ctx, staffID)
		if err != nil {
			return err
		}
		out.Required, out.Enabled, out.EnabledAt = !auth.MFAExempt(st.Role), st.TotpEnabledAt != nil, st.TotpEnabledAt
		if !out.Enabled {
			d := st.MfaRequiredFrom
			out.RequiredFrom = &d
		}
		out.RecoveryCodesLeft, err = q.CountStaffRecoveryCodesLeft(ctx, st.ID)
		return err
	})
	return out, err
}

func (s *Service) audit(ctx context.Context, q *store.Queries, st store.StaffUser, action string, data map[string]any) error {
	return apix.Audit(ctx, q, &st.CompanyID, action, "staff", st.ID.String(), nil, data)
}

// alert prévient le membre du personnel par e-mail d'un changement de sécurité.
func (s *Service) alert(st store.StaffUser, title, message string) {
	if s.Email == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		msg := notify.SecurityEmail(notify.BrandFromConfig(s.Cfg), st.Email, st.FullName, title, message)
		if err := s.Email.SendEmail(ctx, msg); err != nil {
			slog.Error("alerte de sécurité non envoyée", "staff", st.ID, "err", err)
		}
	}()
}

func ptr[T any](v T) *T { return &v }
