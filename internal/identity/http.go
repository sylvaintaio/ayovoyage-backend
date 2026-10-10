package identity

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const tagAuth = "Authentification"
const tagMe = "Mon compte"

// ---------------------------------------------------------------- DTO

type AuthOptions struct {
	Channels []string `json:"channels" doc:"Canaux actifs, par ordre de préférence : email, sms"`
}

// OTPTarget : destinataire du code, par e-mail ou par téléphone.
type OTPTarget struct {
	Email string `json:"email,omitempty" maxLength:"254" format:"email" doc:"Adresse e-mail (connexion par e-mail)"`
	Phone string `json:"phone,omitempty" maxLength:"20" example:"0197000000" doc:"Numéro (connexion par SMS), format local ou E.164"`
}

func (t OTPTarget) target() (Channel, string, error) {
	switch {
	case t.Email != "" && t.Phone != "":
		return "", "", huma.Error422UnprocessableEntity("Indiquez soit un e-mail, soit un numéro de téléphone")
	case t.Email != "":
		return ChannelEmail, t.Email, nil
	case t.Phone != "":
		return ChannelSMS, t.Phone, nil
	}
	return "", "", huma.Error422UnprocessableEntity("Indiquez votre e-mail ou votre numéro de téléphone")
}

type OTPSent struct {
	Channel     string `json:"channel" enum:"email,sms"`
	Destination string `json:"destination" doc:"E-mail ou numéro normalisé"`
	Phone       string `json:"phone,omitempty" doc:"Numéro normalisé (SMS uniquement)"`
	ExpiresIn   int    `json:"expires_in" doc:"Validité du code en secondes"`
	DevCode     string `json:"dev_code,omitempty" doc:"Code (développement avec pilote log uniquement)"`
}

type SessionBody struct {
	AccessToken      string     `json:"access_token,omitempty" doc:"Jeton d'accès (clients mobiles uniquement)"`
	RefreshToken     string     `json:"refresh_token,omitempty" doc:"Jeton de rafraîchissement (clients mobiles uniquement)"`
	AccessExpiresAt  time.Time  `json:"access_expires_at"`
	RefreshExpiresAt time.Time  `json:"refresh_expires_at"`
	SubjectType      string     `json:"subject_type" enum:"customer,staff"`
	SubjectID        uuid.UUID  `json:"subject_id"`
	Role             string     `json:"role,omitempty"`
	CompanyID        *uuid.UUID `json:"company_id,omitempty"`
	AgencyID         *uuid.UUID `json:"agency_id,omitempty"`
	IsNewAccount     bool       `json:"is_new_account,omitempty"`
	MFAVerifiedAt    *time.Time `json:"mfa_verified_at,omitempty" doc:"Personnel : dernière vérification du code de double authentification"`
	MFASetupDeadline *time.Time `json:"mfa_setup_deadline,omitempty" doc:"Personnel sans double authentification : date limite d'activation"`
	RecoveryCodes    []string   `json:"recovery_codes,omitempty" doc:"Codes de secours, renvoyés une seule fois à l'activation"`
}

// StaffLoginBody : soit une session, soit une étape de double authentification.
type StaffLoginBody struct {
	MFAStep      string       `json:"mfa_step,omitempty" enum:"verify,setup" doc:"verify : saisir le code ; setup : activer la double authentification"`
	MFAToken     string       `json:"mfa_token,omitempty" doc:"Jeton d'étape (5 minutes) à renvoyer avec le code"`
	MFAExpiresAt *time.Time   `json:"mfa_expires_at,omitempty"`
	Session      *SessionBody `json:"session,omitempty" doc:"Présent si aucune étape supplémentaire n'est requise"`
}

type SessionOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      SessionBody
}

func (h *Handler) sessionOutput(sess Session, clientType string, isNew bool) *SessionOutput {
	out := &SessionOutput{Body: SessionBody{
		AccessExpiresAt: sess.AccessExpiresAt, RefreshExpiresAt: sess.RefreshExpires,
		SubjectType: string(sess.Principal.Type), SubjectID: sess.Principal.ID,
		Role: sess.Principal.Role, AgencyID: sess.Principal.AgencyID, IsNewAccount: isNew,
		MFAVerifiedAt: sess.Principal.MFAAt, MFASetupDeadline: sess.MFASetupDeadline, RecoveryCodes: sess.RecoveryCodes,
	}}
	if sess.Principal.IsStaff() {
		cid := sess.Principal.CompanyID
		out.Body.CompanyID = &cid
	}
	if clientType == "web" {
		out.SetCookie = h.sessionCookies(sess)
	} else {
		out.Body.AccessToken, out.Body.RefreshToken = sess.AccessToken, sess.RefreshToken
	}
	return out
}

func (h *Handler) cookie(name, value, path string, maxAge time.Duration, httpOnly bool, sameSite http.SameSite) http.Cookie {
	c := http.Cookie{
		Name: name, Value: value, Path: path, Domain: h.S.Cfg.CookieDomain,
		HttpOnly: httpOnly, Secure: h.S.Cfg.CookieSecure, SameSite: sameSite,
	}
	if maxAge < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(maxAge.Seconds())
	}
	return c
}

func (h *Handler) sessionCookies(sess Session) []http.Cookie {
	return []http.Cookie{
		h.cookie(apix.CookieAccess, sess.AccessToken, "/", h.S.Cfg.AccessTokenTTL, true, http.SameSiteLaxMode),
		h.cookie(apix.CookieRefresh, sess.RefreshToken, h.S.Cfg.CookiePathPrefix+"/v1/auth", h.S.Cfg.RefreshTokenTTL, true, http.SameSiteStrictMode),
		// Lisible par Angular (HttpClient) qui le renvoie dans l'en-tête X-XSRF-TOKEN.
		h.cookie(apix.CookieXSRF, auth.NewOpaqueToken(), "/", h.S.Cfg.RefreshTokenTTL, false, http.SameSiteLaxMode),
	}
}

func (h *Handler) clearCookies() []http.Cookie {
	return []http.Cookie{
		h.cookie(apix.CookieAccess, "", "/", -1, true, http.SameSiteLaxMode),
		h.cookie(apix.CookieRefresh, "", h.S.Cfg.CookiePathPrefix+"/v1/auth", -1, true, http.SameSiteStrictMode),
		h.cookie(apix.CookieXSRF, "", "/", -1, false, http.SameSiteLaxMode),
	}
}

type staffLoginOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      StaffLoginBody
}

type Handler struct {
	S *Service
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	// --- Code à usage unique (client)
	huma.Register(api, apix.Op("get-auth-options", http.MethodGet, "/v1/public/auth-options", tagAuth,
		"Modes de connexion client disponibles", apix.Public),
		func(ctx context.Context, _ *struct{}) (*struct{ Body AuthOptions }, error) {
			out := &struct{ Body AuthOptions }{Body: AuthOptions{Channels: []string{}}}
			for _, ch := range []Channel{ChannelEmail, ChannelSMS} {
				if h.S.ChannelEnabled(ch) {
					out.Body.Channels = append(out.Body.Channels, string(ch))
				}
			}
			return out, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("request-otp", http.MethodPost, "/v1/auth/otp/request", tagAuth,
		"Demander un code de connexion (e-mail ou SMS)", apix.Public),
		"Fournir `email` ou `phone` (un seul). Envoie un code à 6 chiffres valable 5 minutes. "+
			"Limites : 3 demandes / 10 min par destinataire, 20 / heure par adresse IP. "+
			"Canaux actifs : `GET /v1/public/auth-options`. En développement, si le pilote d'envoi est `log` (rien n'est réellement envoyé), le code est renvoyé dans `dev_code`."),
		func(ctx context.Context, in *struct{ Body OTPTarget }) (*struct{ Body OTPSent }, error) {
			ch, raw, err := in.Body.target()
			if err != nil {
				return nil, err
			}
			dest, code, err := h.S.RequestOTP(ctx, ch, raw)
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct{ Body OTPSent }{Body: OTPSent{Channel: string(ch), Destination: dest, ExpiresIn: int(otpTTL.Seconds()), DevCode: code}}
			if ch == ChannelSMS {
				out.Body.Phone = dest
			}
			return out, nil
		})

	huma.Register(api, apix.WithDescription(apix.Op("verify-otp", http.MethodPost, "/v1/auth/otp/verify", tagAuth,
		"Valider le code et ouvrir une session client", apix.Public),
		"Même `email` ou `phone` que lors de la demande. Crée le compte client au premier passage. 5 essais maximum par code."),
		func(ctx context.Context, in *struct {
			ClientType string `header:"X-Client-Type" enum:"web,mobile" default:"mobile" doc:"web : jetons en cookies HttpOnly uniquement ; mobile : jetons dans le corps de la réponse"`
			Body       struct {
				OTPTarget
				Code     string `json:"code" pattern:"^[0-9]{6}$"`
				FullName string `json:"full_name,omitempty" maxLength:"120" doc:"Nom complet (utilisé à la création du compte)"`
			}
		}) (*SessionOutput, error) {
			ch, raw, err := in.Body.target()
			if err != nil {
				return nil, err
			}
			sess, isNew, err := h.S.VerifyOTP(ctx, ch, raw, in.Body.Code, in.Body.FullName)
			if err != nil {
				return nil, apix.Err(err)
			}
			return h.sessionOutput(sess, in.ClientType, isNew), nil
		})

	// --- Personnel
	huma.Register(api, apix.WithDescription(apix.Op("staff-login", http.MethodPost, "/v1/auth/staff/login", tagAuth,
		"Connexion du personnel (étape 1 : mot de passe)", apix.Public),
		"Verrouillage de 15 minutes après 5 échecs consécutifs (mot de passe et codes confondus). "+
			"Si `mfa_step` est renvoyé, aucune session n'est ouverte : `verify` → `POST /v1/auth/staff/mfa/verify` ; "+
			"`setup` → `POST /v1/auth/staff/mfa/setup` puis `/activate`. Les rôles de l'application Agent ne sont pas concernés."),
		func(ctx context.Context, in *struct {
			ClientType string `header:"X-Client-Type" enum:"web,mobile" default:"mobile" doc:"web : jetons en cookies HttpOnly uniquement ; mobile : jetons dans le corps de la réponse"`
			Body       struct {
				Email    string `json:"email" format:"email" maxLength:"254"`
				Password string `json:"password" minLength:"8" maxLength:"128"`
			}
		}) (*staffLoginOutput, error) {
			sess, ch, err := h.S.StaffLogin(ctx, in.Body.Email, in.Body.Password)
			if err != nil {
				return nil, apix.Err(err)
			}
			if ch != nil {
				exp := ch.ExpiresAt
				return &staffLoginOutput{Body: StaffLoginBody{MFAStep: ch.Step, MFAToken: ch.Token, MFAExpiresAt: &exp}}, nil
			}
			so := h.sessionOutput(sess, in.ClientType, false)
			return &staffLoginOutput{SetCookie: so.SetCookie, Body: StaffLoginBody{Session: &so.Body}}, nil
		})

	h.registerMFA(api)
	h.registerDeleteAccount(api)

	// --- Rotation / déconnexion
	huma.Register(api, apix.WithDescription(apix.Op("refresh-session", http.MethodPost, "/v1/auth/refresh", tagAuth,
		"Renouveler la session (rotation du jeton)", apix.Public),
		"Web : le jeton est lu dans le cookie `bv_refresh`. Mobile : dans le corps. "+
			"Un jeton déjà utilisé révoque toute la session (détection de vol)."),
		func(ctx context.Context, in *struct {
			ClientType    string `header:"X-Client-Type" enum:"web,mobile" default:"mobile" doc:"web : jetons en cookies HttpOnly uniquement ; mobile : jetons dans le corps de la réponse"`
			RefreshCookie string `cookie:"bv_refresh"`
			Body          *struct {
				RefreshToken string `json:"refresh_token,omitempty"`
			} `required:"false"`
		}) (*SessionOutput, error) {
			tok := in.RefreshCookie
			if in.Body != nil && in.Body.RefreshToken != "" {
				tok = in.Body.RefreshToken
			}
			sess, err := h.S.Refresh(ctx, tok)
			if err != nil {
				return nil, apix.Err(err)
			}
			return h.sessionOutput(sess, in.ClientType, false), nil
		})

	huma.Register(api, apix.Op("logout", http.MethodPost, "/v1/auth/logout", tagAuth, "Se déconnecter", apix.Public),
		func(ctx context.Context, in *struct {
			RefreshCookie string `cookie:"bv_refresh"`
			Body          *struct {
				RefreshToken string `json:"refresh_token,omitempty"`
			} `required:"false"`
		}) (*struct {
			SetCookie []http.Cookie `header:"Set-Cookie"`
		}, error) {
			tok := in.RefreshCookie
			if in.Body != nil && in.Body.RefreshToken != "" {
				tok = in.Body.RefreshToken
			}
			if err := h.S.Logout(ctx, tok); err != nil {
				return nil, apix.Err(err)
			}
			return &struct {
				SetCookie []http.Cookie `header:"Set-Cookie"`
			}{SetCookie: h.clearCookies()}, nil
		})

	h.registerMe(api)
}

// ---------------------------------------------------------------- Mon compte

type CustomerProfile struct {
	ID           uuid.UUID `json:"id"`
	Phone        string    `json:"phone"`
	FullName     string    `json:"full_name"`
	Email        *string   `json:"email,omitempty"`
	BirthDate    *string   `json:"birth_date,omitempty" doc:"AAAA-MM-JJ"`
	ReferralCode string    `json:"referral_code"`
	CreatedAt    time.Time `json:"created_at"`
}

type StaffProfile struct {
	ID        uuid.UUID  `json:"id"`
	CompanyID uuid.UUID  `json:"company_id"`
	AgencyID  *uuid.UUID `json:"agency_id,omitempty"`
	Email     string     `json:"email"`
	FullName  string     `json:"full_name"`
	Phone     string     `json:"phone"`
	Role      string     `json:"role"`
}

type MeBody struct {
	Type     string           `json:"type" enum:"customer,staff"`
	Customer *CustomerProfile `json:"customer,omitempty"`
	Staff    *StaffProfile    `json:"staff,omitempty"`
}

func toCustomerProfile(c store.Customer) *CustomerProfile {
	p := &CustomerProfile{ID: c.ID, Phone: deref(c.Phone), FullName: c.FullName, Email: c.Email,
		ReferralCode: c.ReferralCode, CreatedAt: c.CreatedAt}
	if c.BirthDate != nil {
		s := c.BirthDate.Format(time.DateOnly)
		p.BirthDate = &s
	}
	return p
}

type Passenger struct {
	ID           uuid.UUID `json:"id"`
	FullName     string    `json:"full_name"`
	Phone        string    `json:"phone"`
	Relationship string    `json:"relationship"`
}

// Message : e-mail ou SMS envoyé au client (journal d'activité).
type Message struct {
	ID        int64     `json:"id"`
	Channel   string    `json:"channel" enum:"sms,email,push,whatsapp"`
	Template  string    `json:"template" doc:"Type de message : welcome, booking_confirmed, booking_cancelled, departure_…"`
	Recipient string    `json:"recipient"`
	Status    string    `json:"status" enum:"sent,failed,simulated" doc:"simulated : mode test, rien n'a été envoyé"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body" doc:"Version texte du message"`
	Company   string    `json:"company,omitempty" doc:"Compagnie émettrice (vide pour les messages de la plateforme)"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *Handler) registerMe(api huma.API) {
	db := h.S.DB

	huma.Register(api, apix.Op("get-me", http.MethodGet, "/v1/me", tagMe, "Profil de l'utilisateur connecté", apix.Authenticated),
		func(ctx context.Context, _ *struct{}) (*struct{ Body MeBody }, error) {
			return h.me(ctx)
		})

	huma.Register(api, apix.Op("update-my-profile", http.MethodPut, "/v1/me", tagMe, "Mettre à jour mon profil (client)", apix.Customer),
		func(ctx context.Context, in *struct {
			Body struct {
				FullName  string  `json:"full_name" minLength:"2" maxLength:"120"`
				Email     *string `json:"email,omitempty" format:"email" maxLength:"254"`
				BirthDate *string `json:"birth_date,omitempty" format:"date"`
			}
		}) (*struct{ Body MeBody }, error) {
			p := auth.MustFromContext(ctx)
			var birth *time.Time
			if in.Body.BirthDate != nil && *in.Body.BirthDate != "" {
				t, err := time.Parse(time.DateOnly, *in.Body.BirthDate)
				if err != nil {
					return nil, huma.Error422UnprocessableEntity("Date de naissance invalide")
				}
				birth = &t
			}
			var c store.Customer
			err := db.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				c, err = q.UpdateCustomerProfile(ctx, store.UpdateCustomerProfileParams{
					ID: p.ID, FullName: in.Body.FullName, Email: in.Body.Email, BirthDate: birth,
				})
				return err
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body MeBody }{Body: MeBody{Type: "customer", Customer: toCustomerProfile(c)}}, nil
		})

	huma.Register(api, apix.Op("list-my-messages", http.MethodGet, "/v1/me/messages", tagMe, "Mes messages (e-mails et SMS reçus)", apix.Customer),
		func(ctx context.Context, in *struct {
			Limit int32 `query:"limit" minimum:"1" maximum:"200" default:"100"`
		}) (*struct{ Body []Message }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Message }{Body: []Message{}}
			err := db.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerMessages(ctx, store.ListCustomerMessagesParams{CustomerID: &p.ID, Lim: in.Limit})
				for _, r := range rows {
					out.Body = append(out.Body, Message{ID: r.ID, Channel: r.Channel, Template: r.Template, Recipient: r.Recipient,
						Status: r.Status, Subject: r.Subject, Body: r.Body, Company: deref(r.CompanyName), CreatedAt: r.CreatedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("list-my-passengers", http.MethodGet, "/v1/me/passengers", tagMe, "Mes proches / passagers fréquents", apix.Customer),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Passenger }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Passenger }{Body: []Passenger{}}
			err := db.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerPassengers(ctx, p.ID)
				for _, r := range rows {
					out.Body = append(out.Body, Passenger{ID: r.ID, FullName: r.FullName, Phone: r.Phone, Relationship: r.Relationship})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("create-my-passenger", http.MethodPost, "/v1/me/passengers", tagMe, "Ajouter un proche", apix.Customer),
		func(ctx context.Context, in *struct {
			Body struct {
				FullName     string `json:"full_name" minLength:"2" maxLength:"120"`
				Phone        string `json:"phone,omitempty" maxLength:"20"`
				Relationship string `json:"relationship,omitempty" maxLength:"40" example:"Fille"`
			}
		}) (*struct{ Body Passenger }, error) {
			p := auth.MustFromContext(ctx)
			var r store.CustomerPassenger
			err := db.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				r, err = q.CreateCustomerPassenger(ctx, store.CreateCustomerPassengerParams{
					CustomerID: p.ID, FullName: in.Body.FullName, Phone: in.Body.Phone, Relationship: in.Body.Relationship,
				})
				return err
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			return &struct{ Body Passenger }{Body: Passenger{ID: r.ID, FullName: r.FullName, Phone: r.Phone, Relationship: r.Relationship}}, nil
		})

	huma.Register(api, apix.Op("delete-my-passenger", http.MethodDelete, "/v1/me/passengers/{id}", tagMe, "Supprimer un proche", apix.Customer),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			err := db.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				n, err := q.DeleteCustomerPassenger(ctx, store.DeleteCustomerPassengerParams{ID: in.ID, CustomerID: p.ID})
				if err == nil && n == 0 {
					return apix.NotFound("Proche")
				}
				return err
			})
			return nil, apix.Err(err)
		})
}

func (h *Handler) me(ctx context.Context) (*struct{ Body MeBody }, error) {
	p := auth.MustFromContext(ctx)
	out := &struct{ Body MeBody }{Body: MeBody{Type: string(p.Type)}}
	err := h.S.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		if p.IsCustomer() {
			c, err := q.GetCustomer(ctx, p.ID)
			if err != nil {
				return err
			}
			if c.Status != "active" { // compte supprimé ou bloqué : la session n'est plus valable
				return huma.Error401Unauthorized("Session expirée, veuillez vous reconnecter")
			}
			out.Body.Customer = toCustomerProfile(c)
			return nil
		}
		st, err := q.GetStaff(ctx, p.ID)
		if err != nil {
			return err
		}
		out.Body.Staff = &StaffProfile{ID: st.ID, CompanyID: st.CompanyID, AgencyID: st.AgencyID, Email: st.Email,
			FullName: st.FullName, Phone: st.Phone, Role: st.Role}
		return nil
	})
	return out, apix.Err(err)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
