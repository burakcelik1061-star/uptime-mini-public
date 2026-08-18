package httpapi

import (
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

// Org-scoped yetkilendirme (Faz 1.4).
//
// Kural: yolunda `{orgID}` olan HER uç, isteği yapan kullanıcının o
// organizasyona gerçekten üye olduğunu VERİTABANINDAN doğrulamış olmalı.
// Bu, rotalara tek tek değil chi alt-router'ına takılıyor:
//
//	r.Route("/orgs/{orgID}", func(r chi.Router) {
//	    r.Use(requireOrgMembership(...))   // buradan sonrası org-scoped
//	    r.Get("/", detail)
//	})
//
// Neden tek noktadan: canny-mini'de yetki kontrolü handler'lara dağılmıştı
// ve yeni bir uç eklerken unutmak sessiz bir veri sızıntısı demekti. Burada
// unutmak için alt-router'ın DIŞINA çıkmak gerekiyor — göze çarpan bir hata.

// requireOrgMembership, `{orgID}` yol parçasını çözer, üyeliği veritabanından
// doğrular ve context'e yazar.
//
// Üç durum da AYNI cevabı alır (404): organizasyon yok · uuid biçimsiz ·
// kullanıcı üye değil. "Var ama üye değilsin" ile "yok" arasındaki fark,
// bir saldırganın organizasyon kimliklerini tek tek deneyerek varlık
// haritası çıkarmasına izin verirdi.
func requireOrgMembership(service OrgService, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := auth.IdentityFrom(r.Context())
			if !ok {
				// Buraya düşmek, org-scoped alt-router'ın requireAuth
				// olmadan bağlandığı anlamına gelir.
				logger.Error("org-scoped uç kimlik doğrulaması olmadan bağlanmış",
					slog.String("path", r.URL.Path))
				writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")

				return
			}

			orgID, err := uuid.Parse(chi.URLParam(r, "orgID"))
			if err != nil {
				// Biçimsiz kimlik için de 404: "geçersiz uuid" demek zararsız
				// olurdu ama cevapları ayırmak, ileride yanlışlıkla varlık
				// bilgisi sızdıran bir üçüncü cevaba kapı açıyor.
				writeOrgNotFound(w, r)
				return
			}

			membership, err := service.MembershipFor(r.Context(), orgID, identity.UserID)
			if err != nil {
				if errors.Is(err, orgs.ErrNoAccess) {
					// Reddedilen erişim loglanıyor: veri sızıntısı denemeleri
					// (Faz 7.4'ün hata izleme hikayesi) görünür olsun.
					logger.Info("organizasyona erişim reddedildi",
						slog.String("org_id", orgID.String()),
						slog.String("user_id", identity.UserID.String()))
					writeOrgNotFound(w, r)

					return
				}

				writeInternalError(w, r, logger, "üyelik doğrulanamadı", err)

				return
			}

			next.ServeHTTP(w, r.WithContext(orgs.WithMembership(r.Context(), membership)))
		})
	}
}

// requireOrgRole, org-scoped bir alt-router'da EN AZ verilen rolü şart koşar.
//
// requireOrgMembership'ten SONRA takılır ve veritabanına tekrar gitmez:
// üyelik zaten bu istek için bir kez okundu.
//
// Gereksinim "en az şu rol" biçiminde: owner, admin'in yapabildiği her şeyi
// yapabilir. "İzin verilen roller listesi" yaklaşımında her yeni uçta
// owner'ı listeye eklemeyi unutmak mümkün olurdu.
func requireOrgRole(required store.OrgRole, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			membership, ok := orgs.MembershipFrom(r.Context())
			if !ok {
				// Programlama hatası: rol kontrolü üyelik middleware'i
				// olmadan takılmış. Açık kapı bırakmak yerine 500.
				logger.Error("rol kontrolü üyelik doğrulaması olmadan bağlanmış",
					slog.String("path", r.URL.Path))
				writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

				return
			}

			if !membership.HasAtLeast(required) {
				// 403, 404 DEĞİL: kullanıcı bu organizasyonun üyesi olduğunu
				// zaten biliyor, gizlenecek bir varlık bilgisi yok. Ona
				// "yetkin yetmiyor" demek doğru ve kullanışlı.
				writeError(w, r, http.StatusForbidden, codeForbidden,
					"bu işlem için yetkiniz yetersiz")

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func writeOrgNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir organizasyon yok")
}

// memberResponse, üye listesinin öğesi.
type memberResponse struct {
	UserID   string    `json:"userId"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

// detail — GET /v1/orgs/{orgID}
//
// Veritabanına tekrar gitmiyor: middleware üyeliği okurken organizasyonu da
// getirdi.
func (h *orgHandler) detail(w http.ResponseWriter, r *http.Request) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	writeJSON(w, http.StatusOK, map[string]organizationResponse{
		"organization": {
			ID:        membership.OrgID.String(),
			Name:      membership.OrgName,
			Slug:      membership.OrgSlug,
			CreatedAt: membership.OrgSince,
			Role:      string(membership.Role),
		},
	})
}

// members — GET /v1/orgs/{orgID}/members
func (h *orgHandler) members(w http.ResponseWriter, r *http.Request) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	// Sorgu org_id ile sınırlı ve org_id CONTEXT'ten geliyor — yani
	// middleware'in doğruladığı değerden. Yol parçasını burada yeniden
	// okumak, doğrulanmamış bir değerle sorgu yapma riski demek olurdu.
	rows, err := h.orgs.ListMembers(r.Context(), membership.OrgID)
	if err != nil {
		writeInternalError(w, r, h.logger, "üyeler okunamadı", err)
		return
	}

	members := make([]memberResponse, 0, len(rows))
	for _, row := range rows {
		members = append(members, memberResponse{
			UserID:   row.ID.String(),
			Email:    row.Email,
			Name:     row.Name,
			Role:     string(row.Role),
			JoinedAt: row.JoinedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string][]memberResponse{"members": members})
}
