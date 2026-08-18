package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// DAVET AKIŞI (Faz 8.3).
//
// UÇLAR İKİ AYRI AĞAÇTA ve bu bu adımın belirleyici kararı:
//
//   - Davet YÖNETİMİ (oluştur/listele/iptal) org-scoped ağacın İÇİNDE:
//     çağıran zaten üye ve rolü doğrulanmış. 7.1'in `chi.Walk` testi bu üç
//     ucu kendiliğinden kapsıyor.
//   - Daveti KABUL etmek org-scoped ağacın DIŞINDA olmak ZORUNDA: kabul eden
//     kişi henüz ÜYE DEĞİL, yani `requireOrgMembership` ona 404 döndürürdü.
//     Ucu o ağaca bağlayıp "davet varsa geç" diye bir istisna yazmak, 1.4'ün
//     "kontrolü atlamak için ağacın dışına çıkmak gerekir" garantisini TÜM
//     uçlar için zayıflatırdı — 5.1'in public yüzeyi ayrı bir alt-router'a
//     almasının aynı gerekçesi. Kimliği yoldaki org değil TOKEN taşıyor.
//
// Kabul ucu yine de `requireAuth` altında: katılacak bir hesap gerekiyor.

// InvitationService, davet uçlarının ihtiyaç duyduğu davranışlar.
type InvitationService interface {
	Invite(
		ctx context.Context, inviter orgs.Membership, email string, role store.OrgRole, publicBaseURL string,
	) error
	ListInvitations(ctx context.Context, orgID uuid.UUID) ([]orgs.Invitation, error)
	RevokeInvitation(ctx context.Context, orgID, invitationID uuid.UUID) error
	AcceptInvitation(
		ctx context.Context, userID uuid.UUID, userEmail, rawToken string,
	) (orgs.AcceptedInvitation, error)
}

// UserLookup, kabul akışının kullanıcının adresini okuduğu yer.
//
// ADRES TOKEN'DAN DEĞİL VERİTABANINDAN geliyor: `auth.Identity` bilinçli
// olarak e-posta taşımıyor (1.1'in kararı — token'a konan her alan, veri
// değişse bile token ömrü boyunca eski değeriyle dolaşır). Davet eşleşmesi
// tam da adrese dayandığı için burada güncel değer şart.
type UserLookup interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
}

type invitationHandler struct {
	invitations   InvitationService
	users         UserLookup
	publicBaseURL string
	logger        *slog.Logger
}

// invitationResponse, bekleyen davetin dışa verilen gösterimi.
//
// TOKEN YOK: ekranın onu göstermesi gerekmiyor ve dönen her alan bir sızıntı
// yüzeyi (linki yeniden göstermek, daveti iptal edip yenisini göndermekten
// daha zayıf bir güvenlik hikâyesi olurdu).
type invitationResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	// InvitedByEmail null olabilir: davet eden kullanıcı silinmiş olabilir
	// (migration 00019 → ON DELETE SET NULL).
	InvitedByEmail *string `json:"invitedByEmail"`
}

type createInvitationRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type acceptInvitationRequest struct {
	Token string `json:"token"`
}

// acceptedOrganizationResponse, kabul cevabının gövdesi.
//
// AYRI BİR TİP, `organizationResponse` DEĞİL: kabul akışı organizasyonun
// yalnızca kimliğini, adını ve verilen rolü biliyor. Ortak tipi kullanmak
// `slug: ""` ve `createdAt: "0001-01-01T00:00:00Z"` gibi SIFIR DEĞERLERİ veri
// gibi göndermek olurdu — istemci onları gerçek sanıp gösterebilirdi. Eksik
// alanı doldurmak için ikinci bir sorgu atmak da gereksiz: istemci panoya
// gidince `GET /v1/orgs` zaten tam kaydı okuyor.
type acceptedOrganizationResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func (h *invitationHandler) membership(w http.ResponseWriter, r *http.Request) (orgs.Membership, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return orgs.Membership{}, false
	}

	return membership, true
}

// create — POST /v1/orgs/{orgID}/invitations
//
// YETKİ: admin+ (router'da). Kimin organizasyona girebileceğine karar vermek
// 0.1 §2'nin yetki tablosunda "üye yönetimi" olarak admin'e ait.
func (h *invitationHandler) create(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	var req createInvitationRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	role, err := orgs.ParseRole(req.Role)
	if err != nil {
		writeFieldError(w, r, "role", err.Error())
		return
	}

	// DOĞRULAMA SERVİSTE (2.1'den beri): adres biçimi ve yetki yükseltme
	// kapısı orgs katmanında. Handler yalnızca hata tiplerini HTTP'ye çeviriyor.
	switch err := h.invitations.Invite(
		r.Context(), membership, req.Email, role, h.publicBaseURL,
	); {
	case err == nil:
		// 202 (Accepted), 201 DEĞİL — 6.4'ün abonelik ucuyla aynı gerekçe:
		// sunucu "isteğini aldım" diyor, "davet yazdım" demiyor. Zaten
		// yazılıp yazılmadığı (bekleyen davet zaten varsa yazılmıyor)
		// söylenmeyecek bilgi.
		writeJSON(w, http.StatusAccepted, map[string]string{
			"message": "Davet gönderildi.",
		})

	case errors.Is(err, orgs.ErrInvalidEmail):
		writeFieldError(w, r, "email", err.Error())

	case errors.Is(err, orgs.ErrRoleTooHigh):
		// 403: çağıran organizasyonun üyesi olduğunu zaten biliyor, gizlenecek
		// varlık bilgisi yok (1.4'ün "yetersiz rol 403 alır" kararı).
		writeError(w, r, http.StatusForbidden, codeForbidden, err.Error())

	default:
		writeInternalError(w, r, h.logger, "davet gönderilemedi", err)
	}
}

// list — GET /v1/orgs/{orgID}/invitations
//
// OKUMA ÜYEYE AÇIK: "organizasyona kim davet edilmiş" salt-okunur panonun
// parçası (0.1 §2), tıpkı üye listesi gibi.
func (h *invitationHandler) list(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	rows, err := h.invitations.ListInvitations(r.Context(), membership.OrgID)
	if err != nil {
		writeInternalError(w, r, h.logger, "davetler okunamadı", err)
		return
	}

	invitations := make([]invitationResponse, 0, len(rows))
	for _, row := range rows {
		invitations = append(invitations, invitationResponse{
			ID:             row.ID.String(),
			Email:          row.Email,
			Role:           string(row.Role),
			ExpiresAt:      row.ExpiresAt,
			CreatedAt:      row.CreatedAt,
			InvitedByEmail: row.InvitedByEmail,
		})
	}

	writeJSON(w, http.StatusOK, map[string][]invitationResponse{"invitations": invitations})
}

// remove — DELETE /v1/orgs/{orgID}/invitations/{invitationID}
func (h *invitationHandler) remove(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	invitationID, err := uuid.Parse(chi.URLParam(r, "invitationID"))
	if err != nil {
		// Biçimsiz kimlik ile olmayan kimlik AYNI cevap (2.1'in disiplini).
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir davet yok")
		return
	}

	switch err := h.invitations.RevokeInvitation(r.Context(), membership.OrgID, invitationID); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)

	case errors.Is(err, orgs.ErrInvitationNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir davet yok")

	default:
		writeInternalError(w, r, h.logger, "davet iptal edilemedi", err)
	}
}

// accept — POST /v1/invitations/accept
//
// ORG-SCOPED AĞACIN DIŞINDA (bkz. dosya başı): kabul eden kişi henüz üye
// değil. Kimliği yoldaki org değil, gövdedeki TOKEN taşıyor.
func (h *invitationHandler) accept(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")
		return
	}

	var req acceptInvitationRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	if req.Token == "" {
		writeFieldError(w, r, "token", "zorunlu")
		return
	}

	user, err := h.users.GetUserByID(r.Context(), identity.UserID)
	if err != nil {
		writeInternalError(w, r, h.logger, "kullanıcı okunamadı", err)
		return
	}

	accepted, err := h.invitations.AcceptInvitation(r.Context(), identity.UserID, user.Email, req.Token)

	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]acceptedOrganizationResponse{
			"organization": {
				ID:   accepted.OrgID.String(),
				Name: accepted.OrgName,
				Role: string(accepted.Role),
			},
		})

	case errors.Is(err, orgs.ErrInvitationNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound,
			"davet geçersiz veya daha önce kullanılmış")

	case errors.Is(err, orgs.ErrInvitationExpired):
		// 410 (Gone): kaynak vardı, artık yok. 404'ten AYRILIYOR çünkü
		// kullanıcının yapacağı iş farklı — "yöneticiden yenisini iste".
		writeError(w, r, http.StatusGone, codeInvitationExpired,
			"davetin süresi dolmuş; yöneticinizden yeni bir davet isteyin")

	case errors.Is(err, orgs.ErrInvitationEmailMismatch):
		// 403 ve mesaj hedef adresi YAZMIYOR: linki ele geçiren birine
		// daveti kimin aldığını söylemenin faydası yok.
		writeError(w, r, http.StatusForbidden, codeForbidden,
			"bu davet başka bir e-posta adresi için gönderildi")

	default:
		writeInternalError(w, r, h.logger, "davet kabul edilemedi", err)
	}
}
