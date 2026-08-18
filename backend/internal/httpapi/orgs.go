package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// OrgService, organizasyon uçlarının ihtiyaç duyduğu davranışlar
// (bkz. internal/orgs).
type OrgService interface {
	Create(ctx context.Context, ownerID uuid.UUID, name string) (store.Organization, error)
	ListForUser(ctx context.Context, userID uuid.UUID) ([]store.ListOrganizationsForUserRow, error)
	// MembershipFor, organizasyon yoksa da kullanıcı üye değilse de
	// orgs.ErrNoAccess döndürür (bkz. requireOrgMembership).
	MembershipFor(ctx context.Context, orgID, userID uuid.UUID) (orgs.Membership, error)
	ListMembers(ctx context.Context, orgID uuid.UUID) ([]store.ListOrganizationMembersRow, error)
}

type orgHandler struct {
	orgs   OrgService
	logger *slog.Logger
}

// organizationResponse, organizasyonun dışa verilen gösterimi.
type organizationResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
	// Role, isteği yapan kullanıcının bu organizasyondaki rolü. İstemci
	// "burada ne yapabilirim" sorusunu ikinci bir istekle sormasın.
	Role string `json:"role"`
}

type createOrganizationRequest struct {
	Name string `json:"name"`
}

// create — POST /v1/orgs
//
// Kimlik doğrulaması gerekli; oluşturan kullanıcı otomatik olarak OWNER
// oluyor.
func (h *orgHandler) create(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		h.logger.Error("kimlik doğrulanmamış istek korumalı uca ulaştı",
			slog.String("path", r.URL.Path))
		writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")

		return
	}

	var req createOrganizationRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	name := strings.TrimSpace(req.Name)

	v := &validator{}
	validateName(v, name)
	if apiErr := v.err(); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	// Slug İSTEMCİDEN alınmıyor: adres alanı global (Faz 5.4'te alt alan
	// adı olacak) ve kullanıcıya bırakılırsa doğrulama/çakışma yükü her
	// istemciye dağılırdı. Sunucu üretiyor, çakışmayı da kendisi çözüyor.
	org, err := h.orgs.Create(r.Context(), identity.UserID, name)
	if err != nil {
		writeInternalError(w, r, h.logger, "organizasyon oluşturulamadı", err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]organizationResponse{
		"organization": {
			ID:        org.ID.String(),
			Name:      org.Name,
			Slug:      org.Slug,
			CreatedAt: org.CreatedAt,
			// Oluşturan her zaman owner (bkz. orgs.Service.Create).
			Role: string(store.OrgRoleOwner),
		},
	})
}

// list — GET /v1/orgs
//
// Yalnızca isteği yapan kullanıcının ÜYE OLDUĞU organizasyonlar. Bu ucun
// "hepsini getir" gibi bir hâli bilinçli olarak yok: çok kiracılı bir
// üründe organizasyon listesi başlı başına bir müşteri listesidir.
func (h *orgHandler) list(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		h.logger.Error("kimlik doğrulanmamış istek korumalı uca ulaştı",
			slog.String("path", r.URL.Path))
		writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")

		return
	}

	rows, err := h.orgs.ListForUser(r.Context(), identity.UserID)
	if err != nil {
		writeInternalError(w, r, h.logger, "organizasyonlar okunamadı", err)
		return
	}

	// Boş liste JSON'da `null` değil `[]` olmalı: istemci tarafında
	// "liste yok" ile "liste boş" ayrımı gereksiz bir özel duruma dönüşmesin.
	organizations := make([]organizationResponse, 0, len(rows))
	for _, row := range rows {
		organizations = append(organizations, organizationResponse{
			ID:        row.ID.String(),
			Name:      row.Name,
			Slug:      row.Slug,
			CreatedAt: row.CreatedAt,
			Role:      string(row.Role),
		})
	}

	writeJSON(w, http.StatusOK, map[string][]organizationResponse{"organizations": organizations})
}
