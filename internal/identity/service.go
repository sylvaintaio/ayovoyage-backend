// Package identity gère l'authentification (OTP client, connexion du personnel,
// rotation des jetons) et le profil de l'utilisateur connecté.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/jobs"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/store"
)

const (
	otpTTL           = 5 * time.Minute
	otpMaxAttempts   = 5
	otpPerPhoneLimit = 3  // par tranche de 10 minutes
	otpPerIPLimit    = 20 // par heure
)

type Service struct {
	DB     *database.DB
	Cfg    config.Config
	Tokens *auth.TokenIssuer
	SMS    notify.SMSSender
	Email  notify.EmailSender
	Queue  *jobs.Queue
}

// welcomeDelay : l'e-mail de bienvenue part un peu après la création du compte,
// le temps que le client indique son nom (étape suivante de l'inscription).
const welcomeDelay = time.Minute

// Session : jetons émis après une authentification réussie.
type Session struct {
	Principal       auth.Principal
	AccessToken     string
	AccessExpiresAt time.Time
	RefreshToken    string
	RefreshExpires  time.Time
	// Personnel sans double authentification, en période de grâce : date limite d'activation.
	MFASetupDeadline *time.Time
	// Codes de secours générés à l'activation (affichés une seule fois).
	RecoveryCodes []string
}

func (s *Service) otpHash(phoneNumber, code string) string {
	mac := hmac.New(sha256.New, s.Cfg.OTPSecret)
	mac.Write([]byte(phoneNumber + "|" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// Channel : moyen d'envoi du code (SMS ou e-mail).
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
)

// destination valide et normalise le numéro ou l'adresse selon le canal.
func (s *Service) destination(ch Channel, raw string) (string, error) {
	switch ch {
	case ChannelEmail:
		e := strings.ToLower(strings.TrimSpace(raw))
		addr, err := mail.ParseAddress(e)
		if err != nil || addr.Address != e || !strings.Contains(e[strings.LastIndex(e, "@")+1:], ".") {
			return "", huma.Error422UnprocessableEntity("Adresse e-mail invalide")
		}
		return e, nil
	case ChannelSMS:
		p, err := phone.Normalize(raw, s.Cfg.DefaultCountryCode)
		if err != nil {
			return "", huma.Error422UnprocessableEntity("Numéro de téléphone invalide")
		}
		return p, nil
	}
	return "", huma.Error422UnprocessableEntity("Canal inconnu")
}

// ChannelEnabled indique si un canal de connexion est activé (OTP_CHANNELS).
func (s *Service) ChannelEnabled(ch Channel) bool {
	return slices.Contains(s.Cfg.OTPChannels, string(ch))
}

// RequestOTP génère et envoie un code à usage unique par SMS ou par e-mail.
// Renvoie la destination normalisée, et le code en développement uniquement.
func (s *Service) RequestOTP(ctx context.Context, ch Channel, raw string) (string, string, error) {
	if !s.ChannelEnabled(ch) {
		return "", "", huma.Error422UnprocessableEntity("Ce mode de connexion n'est pas disponible")
	}
	dest, err := s.destination(ch, raw)
	if err != nil {
		return "", "", err
	}
	ip := apix.Meta(ctx).IP
	code := refgen.Digits(6)
	err = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		n, err := q.CountOTPsByDestinationSince(ctx, store.CountOTPsByDestinationSinceParams{Destination: dest, CreatedAt: time.Now().Add(-10 * time.Minute)})
		if err != nil {
			return err
		}
		if n >= otpPerPhoneLimit {
			return huma.Error429TooManyRequests("Trop de demandes de code. Réessayez dans quelques minutes.")
		}
		if ip != "" {
			n, err = q.CountOTPsByIPSince(ctx, store.CountOTPsByIPSinceParams{RequesterIp: ip, CreatedAt: time.Now().Add(-time.Hour)})
			if err != nil {
				return err
			}
			if n >= otpPerIPLimit {
				return huma.Error429TooManyRequests("Trop de demandes depuis cette adresse. Réessayez plus tard.")
			}
		}
		_, err = q.CreateOTP(ctx, store.CreateOTPParams{
			Destination: dest, Channel: string(ch), CodeHash: s.otpHash(dest, code),
			ExpiresAt: time.Now().Add(otpTTL), RequesterIp: ip,
		})
		return err
	})
	if err != nil {
		return "", "", err
	}
	// Envoi synchrone : le code n'est jamais stocké en clair (pas de file d'attente).
	if ch == ChannelEmail {
		err = s.Email.SendEmail(ctx, notify.OTPEmail(notify.BrandFromConfig(s.Cfg), dest, code, int(otpTTL.Minutes())))
	} else {
		err = s.SMS.SendSMS(ctx, dest, "Votre code de connexion "+s.Cfg.BrandName+" : "+code+". Il expire dans 5 minutes. Ne le communiquez à personne.")
	}
	if err != nil {
		slog.Error("envoi OTP", "canal", ch, "err", err)
		if ch == ChannelEmail {
			return "", "", huma.Error503ServiceUnavailable("Envoi de l'e-mail impossible pour le moment, réessayez")
		}
		return "", "", huma.Error503ServiceUnavailable("Envoi du SMS impossible, réessayez")
	}
	// Le code n'est renvoyé qu'en développement et quand rien n'est réellement
	// envoyé (pilote « log ») : un code parti par e-mail ou SMS reste secret.
	if s.Cfg.IsDev() && s.driver(ch) == "log" {
		return dest, code, nil
	}
	return dest, "", nil
}

func (s *Service) driver(ch Channel) string {
	if ch == ChannelEmail {
		return s.Cfg.EmailDriver
	}
	return s.Cfg.SMS.Driver
}

var errOTPInvalid = huma.Error400BadRequest("Code invalide ou expiré")

// VerifyOTP valide le code et ouvre une session client (création du compte au besoin).
func (s *Service) VerifyOTP(ctx context.Context, ch Channel, raw, code, fullName string) (Session, bool, error) {
	dest, err := s.destination(ch, raw)
	if err != nil {
		return Session{}, false, errOTPInvalid
	}
	var (
		customerID uuid.UUID
		inserted   bool
		authErr    error // erreur métier : la transaction est quand même validée (compteur d'essais)
	)
	err = s.DB.Tx(ctx, database.System(), func(tx pgx.Tx, q *store.Queries) error {
		otp, err := q.GetLatestActiveOTP(ctx, dest)
		if database.IsNotFound(err) {
			authErr = errOTPInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if otp.Attempts >= otpMaxAttempts {
			authErr = huma.Error429TooManyRequests("Nombre d'essais dépassé, demandez un nouveau code")
			return nil
		}
		if !hmac.Equal([]byte(otp.CodeHash), []byte(s.otpHash(dest, code))) {
			authErr = errOTPInvalid
			return q.IncrementOTPAttempts(ctx, otp.ID)
		}
		if err := q.ConsumeOTP(ctx, otp.ID); err != nil {
			return err
		}
		status := ""
		if ch == ChannelEmail {
			c, err := q.UpsertCustomerByEmail(ctx, store.UpsertCustomerByEmailParams{Email: dest, FullName: fullName, ReferralCode: refgen.Referral()})
			if err != nil {
				return err
			}
			customerID, inserted, status = c.ID, c.Inserted, c.Status
		} else {
			c, err := q.UpsertCustomerByPhone(ctx, store.UpsertCustomerByPhoneParams{Phone: &dest, FullName: fullName, ReferralCode: refgen.Referral()})
			if err != nil {
				return err
			}
			customerID, inserted, status = c.ID, c.Inserted, c.Status
		}
		if status != "active" {
			authErr = huma.Error403Forbidden("Ce compte est désactivé")
			return nil
		}
		if inserted && s.Queue != nil {
			at := time.Now().Add(welcomeDelay)
			if strings.TrimSpace(fullName) != "" {
				at = time.Now()
			}
			return s.Queue.InsertTxOpts(ctx, tx, jobs.WelcomeArgs{CustomerID: customerID}, &river.InsertOpts{ScheduledAt: at})
		}
		return nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if authErr != nil {
		return Session{}, false, authErr
	}
	sess, err := s.openSession(ctx, auth.Principal{Type: auth.SubjectCustomer, ID: customerID}, uuid.Nil)
	return sess, inserted, err
}

var (
	errBadCredentials = huma.Error401Unauthorized("E-mail ou mot de passe incorrect")
	errLocked         = huma.Error429TooManyRequests("Compte temporairement verrouillé après plusieurs échecs. Réessayez dans 15 minutes.")
)

// StaffLogin vérifie le mot de passe du personnel (verrouillage après 5 échecs).
// Si la double authentification est active ou obligatoire, aucune session
// n'est ouverte : un jeton d'étape permet seulement de saisir le code.
func (s *Service) StaffLogin(ctx context.Context, email, password string) (Session, *MFAChallenge, error) {
	var (
		staff   store.StaffUser
		authErr error
	)
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		staff, err = q.GetStaffByEmail(ctx, email)
		if database.IsNotFound(err) {
			_, _ = auth.VerifyPassword(password, auth.DummyHash())
			authErr = errBadCredentials
			return nil
		}
		if err != nil {
			return err
		}
		if staff.LockedUntil != nil && staff.LockedUntil.After(time.Now()) {
			authErr = errLocked
			return nil
		}
		ok, err := auth.VerifyPassword(password, staff.PasswordHash)
		if err != nil {
			return err
		}
		if !ok || !staff.IsActive {
			authErr = errBadCredentials
			return q.RecordStaffLoginFailure(ctx, staff.ID)
		}
		if s.mfaStep(staff) != "" {
			return nil // le compteur d'échecs n'est remis à zéro qu'après le code
		}
		return q.RecordStaffLoginSuccess(ctx, staff.ID)
	})
	if err != nil {
		return Session{}, nil, err
	}
	if authErr != nil {
		return Session{}, nil, authErr
	}
	if step := s.mfaStep(staff); step != "" {
		ch, err := s.challenge(staff, step)
		return Session{}, ch, err
	}
	sess, err := s.openSession(ctx, staffPrincipal(staff), uuid.Nil)
	if err == nil && !auth.MFAExempt(staff.Role) && staff.TotpEnabledAt == nil {
		d := staff.MfaRequiredFrom
		sess.MFASetupDeadline = &d // période de grâce : à activer avant cette date
	}
	return sess, nil, err
}

func staffPrincipal(st store.StaffUser) auth.Principal {
	return auth.Principal{Type: auth.SubjectStaff, ID: st.ID, CompanyID: st.CompanyID, AgencyID: st.AgencyID, Role: st.Role}
}

// openSession émet un jeton d'accès et un jeton de rafraîchissement.
// familyID != Nil lors d'une rotation (même famille de jetons).
func (s *Service) openSession(ctx context.Context, p auth.Principal, familyID uuid.UUID) (Session, error) {
	access, exp, err := s.Tokens.Issue(p)
	if err != nil {
		return Session{}, err
	}
	refresh := auth.NewOpaqueToken()
	if familyID == uuid.Nil {
		familyID = uuid.New()
	}
	m := apix.Meta(ctx)
	refreshExp := time.Now().Add(s.Cfg.RefreshTokenTTL)
	err = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		_, err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
			SubjectType: string(p.Type), SubjectID: p.ID, FamilyID: familyID,
			TokenHash: auth.HashToken(refresh), ExpiresAt: refreshExp,
			UserAgent: truncate(m.UserAgent, 300), Ip: m.IP, MfaAt: p.MFAAt,
		})
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return Session{Principal: p, AccessToken: access, AccessExpiresAt: exp, RefreshToken: refresh, RefreshExpires: refreshExp}, nil
}

var errRefreshInvalid = huma.Error401Unauthorized("Session expirée, veuillez vous reconnecter")

// Refresh effectue la rotation du jeton. La réutilisation d'un jeton déjà
// utilisé (vol probable) révoque toute la famille.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	if refreshToken == "" {
		return Session{}, errRefreshInvalid
	}
	var (
		principal auth.Principal
		familyID  uuid.UUID
		oldID     uuid.UUID
		authErr   error
	)
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		rt, err := q.GetRefreshTokenByHash(ctx, auth.HashToken(refreshToken))
		if database.IsNotFound(err) {
			authErr = errRefreshInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil {
			slog.Warn("réutilisation d'un jeton de rafraîchissement révoqué : famille révoquée",
				"subject", rt.SubjectID, "family", rt.FamilyID)
			authErr = errRefreshInvalid
			return q.RevokeRefreshFamily(ctx, rt.FamilyID)
		}
		if rt.ExpiresAt.Before(time.Now()) {
			authErr = errRefreshInvalid
			return nil
		}
		switch auth.SubjectType(rt.SubjectType) {
		case auth.SubjectCustomer:
			c, err := q.GetCustomer(ctx, rt.SubjectID)
			if err != nil {
				return err
			}
			if c.Status != "active" {
				authErr = errRefreshInvalid
				return q.RevokeRefreshFamily(ctx, rt.FamilyID)
			}
			principal = auth.Principal{Type: auth.SubjectCustomer, ID: c.ID}
		case auth.SubjectStaff:
			st, err := q.GetStaff(ctx, rt.SubjectID)
			if err != nil {
				return err
			}
			if !st.IsActive {
				authErr = errRefreshInvalid
				return q.RevokeRefreshFamily(ctx, rt.FamilyID)
			}
			principal = staffPrincipal(st) // rôle et agence rechargés à chaque rotation
			principal.MFAAt = rt.MfaAt
		}
		familyID, oldID = rt.FamilyID, rt.ID
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if authErr != nil {
		return Session{}, authErr
	}
	sess, err := s.openSession(ctx, principal, familyID)
	if err != nil {
		return Session{}, err
	}
	err = s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		rt, err := q.GetRefreshTokenByHash(ctx, auth.HashToken(sess.RefreshToken))
		if err != nil {
			return err
		}
		return q.RotateRefreshToken(ctx, store.RotateRefreshTokenParams{ID: oldID, ReplacedBy: &rt.ID})
	})
	return sess, err
}

// Logout révoque la famille du jeton de rafraîchissement présenté.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	return s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		rt, err := q.GetRefreshTokenByHash(ctx, auth.HashToken(refreshToken))
		if database.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return q.RevokeRefreshFamily(ctx, rt.FamilyID)
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
