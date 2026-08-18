-- name: CreateOrganization :one
INSERT INTO organizations (name, slug)
VALUES ($1, $2)
-- `plan` de dönüyor: satır tam olarak `store.Organization` şeklinde kalsın
-- (yeni sütun eklendiğinde çağıranın tipi sessizce değişmesin).
RETURNING id, name, slug, created_at, plan;

-- name: AddOrganizationMember :one
INSERT INTO organization_members (org_id, user_id, role)
VALUES ($1, $2, $3)
RETURNING id, org_id, user_id, role, joined_at;

-- name: GetOrganizationForMember :one
-- Org-scoped yetkilendirmenin tek sorgusu: organizasyonu YALNIZCA istekte
-- bulunan kullanıcı üyeyse döndürür.
--
-- "Önce organizasyonu getir, sonra üyeliğe bak" YAPILMIYOR — o kurgu iki
-- ayrı sonuç (yok / üye değilsin) üretir ve aradaki farkı dışarı sızdırmak
-- kolaylaşır. Tek satırlık cevap: ya erişebiliyorsun ya da senin için o
-- organizasyon yok.
SELECT o.id, o.name, o.slug, o.plan, o.created_at, m.role
FROM organizations o
JOIN organization_members m ON m.org_id = o.id
WHERE o.id = $1 AND m.user_id = $2;

-- name: ListOrganizationMembers :many
SELECT u.id, u.email, u.name, m.role, m.joined_at
FROM organization_members m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1
ORDER BY m.joined_at;

-- name: ListOrganizationsForUser :many
-- Kullanıcının üyesi OLDUĞU organizasyonlar; rol de aynı sorgudan geliyor
-- (istemci "bu org'da ne yapabilirim" sorusunu ikinci bir istekle sormasın).
SELECT o.id, o.name, o.slug, o.plan, o.created_at, m.role
FROM organizations o
JOIN organization_members m ON m.org_id = o.id
WHERE m.user_id = $1
ORDER BY o.created_at;

-- name: LockOrganizationForQuota :one
-- Kota kontrolü için organizasyon satırını KİLİTLER (8.5).
--
-- NEDEN KİLİT: "önce say, sınırın altındaysa yaz" iki eşzamanlı istekte
-- ikisini de geçirirdi ve organizasyon kotasının ÜSTÜNE çıkardı — 1.3'ün
-- slug, 4.1'in incident ve 6.3'ün bildirim çakışmasında kapatılan aynı
-- yarış. Orada hakem UNIQUE kısıtıydı; bir SAYIM kısıtla ifade edilemediği
-- için burada hakem satır kilidi: aynı org'a eşzamanlı monitor ekleyenler
-- bu satırda sıraya giriyor. (Kilit kaldırıldığında testi düşüyor:
-- internal/monitors → TestConcurrentCreatesCannotExceedTheLimit.)
--
-- Kilit ORGANİZASYON satırında, monitor tablosunda değil: sayılacak şey
-- henüz var olmayan satır ve "gelecekteki satırlar" kilitlenemez.
SELECT id, plan FROM organizations
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: CountMonitorsForOrg :one
-- Organizasyonun monitor sayısı (kota kontrolü).
--
-- DURAKLATILMIŞ MONITOR'LER DE SAYILIYOR: kota bir KAYIT limiti, bir
-- "çalışan iş" limiti değil. Duraklatılmışı saymamak, kotayı doldurmuş bir
-- kullanıcının hepsini duraklatıp sınırsız monitor tanımlamasına izin
-- verirdi — ve o kayıtlar istenince tek tıkla geri açılırdı.
SELECT count(*) FROM monitors
WHERE org_id = sqlc.arg(org_id);

-- name: GetOrganizationPlan :one
-- Organizasyonun planı (kota kontrolü, kilitsiz).
--
-- Güncelleme yolunda (monitors.Update) SAYIM yapılmıyor — güncelleme yeni
-- kayıt üretmiyor — yani kilide gerek yok. Kotasını doldurmuş bir
-- organizasyonun mevcut monitor'ünü düzenleyebilmesi ŞART: sınır yeni bir
-- şey EKLEMEYİ durduruyor, var olanı yönetmeyi değil.
SELECT plan FROM organizations WHERE id = sqlc.arg(id);

-- name: UpdateOrganizationPlan :one
-- Organizasyonun planını değiştirir (8.5'in mock ödemesi).
UPDATE organizations
SET plan = sqlc.arg(plan)
WHERE id = sqlc.arg(id)
RETURNING id, name, slug, created_at, plan;
