package monitors

import "github.com/burakcelik1061-star/uptime-mini/backend/internal/store"

// Patch, kısmi güncellemenin girdisi: nil olan alan "dokunma" demek.
//
// İşaretçi kullanılmasının sebebi, sıfır değerle "gönderilmedi"yi ayırmak.
// `isActive: false` ile alanın hiç gönderilmemesi aynı şey değil — bool'da
// bu ayrım işaretçi olmadan yapılamaz (JSON'da `0` gönderilen sayısal
// alanlar için de aynısı geçerli: doğrulamaya girip hata almalılar, sessizce
// varsayılana dönmemeliler).
type Patch struct {
	Name                 *string
	URL                  *string
	Method               *store.MonitorMethod
	CheckIntervalSeconds *int32
	TimeoutMs            *int32
	ExpectedStatusMin    *int32
	ExpectedStatusMax    *int32
	FailureThreshold     *int32
	RecoveryThreshold    *int32
	IsActive             *bool

	// DegradedResponseMs ÜÇ DURUMLU olduğu için ayrı bir bayrağı var: alan
	// "gönderilmedi", "null gönderildi" (eşiği kaldır) veya "sayı gönderildi"
	// olabilir. Diğer alanlarda işaretçinin nil olması "gönderilmedi"
	// demek — burada nil aynı zamanda geçerli bir DEĞER (eşik yok), yani tek
	// bir işaretçi ikisini ayıramıyor.
	DegradedResponseMs    *int32
	SetDegradedResponseMs bool
}

// IsEmpty, hiçbir alanın gönderilmediğini söyler.
func (p Patch) IsEmpty() bool {
	return p.Name == nil &&
		p.URL == nil &&
		p.Method == nil &&
		p.CheckIntervalSeconds == nil &&
		p.TimeoutMs == nil &&
		p.ExpectedStatusMin == nil &&
		p.ExpectedStatusMax == nil &&
		p.FailureThreshold == nil &&
		p.RecoveryThreshold == nil &&
		p.IsActive == nil &&
		!p.SetDegradedResponseMs
}

// apply, gönderilen alanları mevcut ayarların üzerine bindirir.
func (p Patch) apply(s Settings) Settings {
	if p.Name != nil {
		s.Name = *p.Name
	}

	if p.URL != nil {
		s.URL = *p.URL
	}

	if p.Method != nil {
		s.Method = *p.Method
	}

	if p.CheckIntervalSeconds != nil {
		s.CheckIntervalSeconds = *p.CheckIntervalSeconds
	}

	if p.TimeoutMs != nil {
		s.TimeoutMs = *p.TimeoutMs
	}

	if p.ExpectedStatusMin != nil {
		s.ExpectedStatusMin = *p.ExpectedStatusMin
	}

	if p.ExpectedStatusMax != nil {
		s.ExpectedStatusMax = *p.ExpectedStatusMax
	}

	if p.FailureThreshold != nil {
		s.FailureThreshold = *p.FailureThreshold
	}

	if p.RecoveryThreshold != nil {
		s.RecoveryThreshold = *p.RecoveryThreshold
	}

	if p.IsActive != nil {
		s.IsActive = *p.IsActive
	}

	if p.SetDegradedResponseMs {
		s.DegradedResponseMs = p.DegradedResponseMs
	}

	return s
}
