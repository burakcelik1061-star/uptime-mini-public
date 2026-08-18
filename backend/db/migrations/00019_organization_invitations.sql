-- +goose Up
-- DAVET AKIŞI (Faz 8.3).
--
-- NEDEN VAR: 1.3 üyeliği veritabanı seviyesinde kurmuştu ve davet akışını
-- açıkça MVP dışına almıştı ("şema hazır"). Sonuç, 1.4'ün özenle kurduğu
-- çok kiracılılığın pratikte TEK KULLANICILI kalmasıydı: ikinci bir kişi
-- organizasyona ancak elle `INSERT` ile girebiliyordu.
CREATE TABLE organization_invitations (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    org_id   uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    -- DAVET BİR KULLANICIYA DEĞİL BİR ADRESE AİT ve bu bilinçli: davet edilen
    -- kişi henüz kaydolmamış olabilir (akışın asıl amacı da bu). `user_id`
    -- yabancı anahtarı olsaydı, davet etmeden önce kullanıcının var olup
    -- olmadığına bakmak gerekirdi — yani uç, PLATFORM GENELİNDE bir "bu adres
    -- kayıtlı mı" sorgulayıcısına dönüşürdü (bkz. invitations.go).
    email    text NOT NULL CHECK (char_length(email) BETWEEN 3 AND 254),

    -- 00001'deki enum: owner | admin | member. Rolün daveti VERENİN rolünü
    -- aşamaması uygulamada zorlanıyor (orgs.Membership.HasAtLeast) — bu bir
    -- satır kısıtı değil, iki satır arasındaki ilişki.
    role     org_role NOT NULL,

    -- TOKEN HAM SAKLANMIYOR (1.2'nin refresh token ve 6.4'ün onay token'ı
    -- kararının aynısı): veritabanını okuyabilen biri, başkasının
    -- organizasyonuna katılma yeteneğini hazır bulmasın.
    --
    -- NULL OLABİLİR ve NULL "bu davet KULLANILDI" demek (6.4'ün onay
    -- token'ıyla aynı): tek kullanımlık olmanın karşılığı, token'ı silmek.
    token_hash text UNIQUE,

    -- Daveti kimin gönderdiği. ON DELETE SET NULL: davet eden kişi hesabını
    -- silse bile davet kaydı (ve kabul edilmişse üyelik) ayakta kalmalı —
    -- 0.1 §7'nin "geçmiş kayıt silinmez" kuralının bu tablodaki karşılığı.
    invited_by uuid REFERENCES users(id) ON DELETE SET NULL,

    -- SÜRELİ: süresiz bir davet linki, bir kez sızdığında sonsuza kadar
    -- geçerli bir organizasyon anahtarı demekti.
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- "AYNI ADRESE İKİ BEKLEYEN DAVET OLMASIN" GARANTİSİ ŞEMADA, uygulamada
-- değil (4.1'in kısmi unique indeksi ve 1.3'ün slug kısıtıyla aynı disiplin):
-- "önce SELECT et, yoksa yaz" iki eşzamanlı istekte iki davet üretirdi ve
-- ikisinin de token'ı geçerli olurdu. Kısmi: kabul edilmiş davetler kayıtta
-- kalıyor (kim ne zaman katıldı) ama yeni bir davete engel değil.
CREATE UNIQUE INDEX organization_invitations_pending_idx
    ON organization_invitations (org_id, email)
    WHERE accepted_at IS NULL;

-- Organizasyonun bekleyen davetlerini listeleyen ekranın indeksi.
CREATE INDEX organization_invitations_org_id_idx
    ON organization_invitations (org_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS organization_invitations;
