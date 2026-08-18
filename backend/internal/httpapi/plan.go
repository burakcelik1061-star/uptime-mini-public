package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// PLAN & KOTA (Faz 8.5) — org-scoped.
//
// Uçlar `requireOrgMembership` ağacının içinde, yani 7.1'in `chi.Walk` testi
// ikisini de kendiliğinden kapsıyor.

// PlanService, plan uçlarının ihtiyaç duyduğu davranışlar.
type PlanService interface {
	PlanFor(ctx context.Context, membership orgs.Membership) (orgs.PlanUsage, error)
	ChangePlan(
		ctx context.Context, membership orgs.Membership, target store.OrgPlan,
	) (store.Organization, error)
}

type planHandler struct {
	plans  PlanService
	logger *slog.Logger
}

// planLimitsResponse, bir planın sınırları.
type planLimitsResponse struct {
	MaxMonitors             int   `json:"maxMonitors"`
	MinCheckIntervalSeconds int32 `json:"minCheckIntervalSeconds"`
}

// planOptionResponse, seçilebilir bir plan.
//
// TÜM PLANLAR DÖNÜYOR, yalnızca mevcut olan değil: ekranın "yükseltirsem ne
// kazanırım" sorusunu cevaplayabilmesi için karşılaştırma gerekiyor ve o
// listeyi istemcide sabitlemek, sunucudaki sayıların ikinci bir kopyası
// olurdu (2.1'in "doğrulama kuralları istemcide tekrarlanmaz" kararının
// kota tarafındaki karşılığı).
type planOptionResponse struct {
	Plan   string             `json:"plan"`
	Label  string             `json:"label"`
	Limits planLimitsResponse `json:"limits"`
}

type planResponse struct {
	Plan   string             `json:"plan"`
	Label  string             `json:"label"`
	Limits planLimitsResponse `json:"limits"`
	// Usage, organizasyonun bugünkü kullanımı.
	Usage planUsageResponse `json:"usage"`
	// Available, seçilebilir planlar ve sınırları.
	Available []planOptionResponse `json:"available"`
}

type planUsageResponse struct {
	Monitors int64 `json:"monitors"`
}

type changePlanRequest struct {
	Plan string `json:"plan"`
}

func limitsResponse(l plans.Limits) planLimitsResponse {
	return planLimitsResponse{
		MaxMonitors:             l.MaxMonitors,
		MinCheckIntervalSeconds: l.MinCheckIntervalSeconds,
	}
}

func availablePlans() []planOptionResponse {
	all := plans.All()
	out := make([]planOptionResponse, 0, len(all))
	for _, plan := range all {
		out = append(out, planOptionResponse{
			Plan:   string(plan),
			Label:  plans.Label(plan),
			Limits: limitsResponse(plans.For(plan)),
		})
	}

	return out
}

func (h *planHandler) membership(w http.ResponseWriter, r *http.Request) (orgs.Membership, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return orgs.Membership{}, false
	}

	return membership, true
}

// get — GET /v1/orgs/{orgID}/plan
//
// OKUMA ÜYEYE AÇIK: "hangi plandayız, kotanın neresindeyiz" salt-okunur
// panonun parçası (0.1 §2).
func (h *planHandler) get(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	usage, err := h.plans.PlanFor(r.Context(), membership)
	if err != nil {
		writeInternalError(w, r, h.logger, "plan okunamadı", err)
		return
	}

	writeJSON(w, http.StatusOK, planResponse{
		Plan:      string(usage.Plan),
		Label:     plans.Label(usage.Plan),
		Limits:    limitsResponse(usage.Limits),
		Usage:     planUsageResponse{Monitors: usage.Monitors},
		Available: availablePlans(),
	})
}

// change — PUT /v1/orgs/{orgID}/plan
//
// YETKİ: OWNER. Admin değil — plan değiştirmek organizasyonun faturasını
// değiştiren bir karar ve 0.1 §2'nin yetki tablosunda admin'in yapamadığı
// şeyler ("org silme, ownership devri") aynı sınıfta. Bu, `requireOrgRole`un
// owner ile ilk kullanımı.
func (h *planHandler) change(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	var req changePlanRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	target, err := plans.Parse(req.Plan)
	if err != nil {
		writeFieldError(w, r, "plan", "plan free veya pro olmalı")
		return
	}

	org, err := h.plans.ChangePlan(r.Context(), membership, target)

	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]planChangeResponse{
			"organization": {
				ID:    org.ID.String(),
				Plan:  string(org.Plan),
				Label: plans.Label(org.Plan),
			},
		})

	case errors.Is(err, orgs.ErrSamePlan):
		writeError(w, r, http.StatusConflict, codePlanUnchanged,
			"organizasyon zaten bu planda")

	case errors.Is(err, orgs.ErrNoAccess):
		writeOrgNotFound(w, r)

	default:
		writeInternalError(w, r, h.logger, "plan değiştirilemedi", err)
	}
}

// planChangeResponse, plan değişikliğinin cevabı.
//
// DAR bir gövde (8.3'ün `acceptedOrganizationResponse`ıyla aynı gerekçe):
// çağıranın bilmesi gereken tek şey yeni plan. Tam organizasyon kaydı
// `GET /v1/orgs` ile zaten okunuyor.
type planChangeResponse struct {
	ID    string `json:"id"`
	Plan  string `json:"plan"`
	Label string `json:"label"`
}
