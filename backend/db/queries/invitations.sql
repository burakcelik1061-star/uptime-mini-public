-- Organizasyon davetleri (Faz 8.3).

-- name: CreateInvitation :one
-- Yeni davet yazar.
--
-- ÇAKIŞMA HAKEMİ ŞEMA: aynı adrese bekleyen ikinci bir davet, kısmi unique
-- indeksin `23505`i ile reddediliyor (1.3'ün slug döngüsüyle aynı disiplin).
-- Uygulama o hatayı "davet zaten var" olarak YORUMLUYOR ama çağırana
-- göstermiyor — 6.4'ün enumeration kararı (bkz. invitations.go).
INSERT INTO organization_invitations (org_id, email, role, token_hash, invited_by, expires_at)
VALUES (
    sqlc.arg(org_id), sqlc.arg(email), sqlc.arg(role),
    sqlc.arg(token_hash), sqlc.arg(invited_by), sqlc.arg(expires_at)
)
RETURNING *;

-- name: ListPendingInvitations :many
-- Organizasyonun BEKLEYEN davetleri.
--
-- `token_hash` DÖNMÜYOR: ekranın onu göstermesi gerekmiyor ve dönen her alan
-- bir sızıntı yüzeyi. Süresi geçmişler de listede — ekran "süresi doldu"
-- diyebilsin ve kullanıcı yenisini gönderebilsin.
SELECT i.id, i.email, i.role, i.expires_at, i.created_at, u.email AS invited_by_email
FROM organization_invitations i
LEFT JOIN users u ON u.id = i.invited_by
WHERE i.org_id = sqlc.arg(org_id) AND i.accepted_at IS NULL
ORDER BY i.created_at DESC;

-- name: DeleteInvitation :execrows
-- Daveti iptal eder.
--
-- `org_id` KOŞULU ŞART (1.4'ün disiplini): kimlik yoldan geliyor, sahiplik
-- sorgunun içinde doğrulanıyor. Yabancı bir davet kimliği 0 satır etkiliyor.
DELETE FROM organization_invitations
WHERE id = sqlc.arg(id) AND org_id = sqlc.arg(org_id);

-- name: GetInvitationByTokenHash :one
-- Kabul akışının tek okuması.
--
-- SÜRE VE KULLANILMIŞLIK BURADA SÜZÜLMÜYOR, çağıran ayırıyor: "süresi dolmuş"
-- ile "hiç yok" farkı KULLANICIYA anlamlı bir mesaj vermek için gerekli
-- (yenisini iste vs. linkin yanlış). Token zaten tahmin edilemez olduğu için
-- bu ayrım bir sızıntı değil — 6.4'ün abonelik ucundaki durum farklıydı,
-- orada çağıran kimliksiz bir YABANCI olabiliyordu.
SELECT i.id, i.org_id, i.email, i.role, i.expires_at, i.accepted_at, o.name AS org_name
FROM organization_invitations i
JOIN organizations o ON o.id = i.org_id
WHERE i.token_hash = sqlc.arg(token_hash);

-- name: AcceptInvitation :execrows
-- Daveti KULLANILMIŞ olarak işaretler ve token'ı düşürür.
--
-- `accepted_at IS NULL` koşulu YARIŞ KAPATIYOR: aynı linke iki kez tıklayan
-- (ya da iki sekmede açan) kullanıcıda ikinci çağrı 0 satır etkiliyor, yani
-- transaction geri alınıyor. Token'ın NULL'a inmesi tek kullanımlığı
-- kalıcı kılıyor (6.4'ün onay token'ıyla aynı).
UPDATE organization_invitations
SET accepted_at = now(), token_hash = NULL
WHERE id = sqlc.arg(id) AND accepted_at IS NULL;

-- name: AddOrganizationMemberIfAbsent :execrows
-- Daveti kabul eden kullanıcıyı üye yapar.
--
-- `ON CONFLICT DO NOTHING` ve MEVCUT ROL KORUNUYOR (`DO UPDATE` değil):
-- davet gönderildikten sonra bir admin o kişiyi elle başka bir rolle eklemiş
-- olabilir ve linke tıklamak, o DAHA YENİ yönetim kararını sessizce ezmek
-- olurdu. Kullanıcının amacı ("organizasyona gireyim") zaten karşılanmış
-- durumda; rol değişimi ayrı ve görünür bir hareket olmalı.
INSERT INTO organization_members (org_id, user_id, role)
VALUES (sqlc.arg(org_id), sqlc.arg(user_id), sqlc.arg(role))
ON CONFLICT (org_id, user_id) DO NOTHING;
