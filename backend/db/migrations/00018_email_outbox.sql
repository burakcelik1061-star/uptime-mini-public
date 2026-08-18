-- +goose Up
-- GELİŞTİRİCİ POSTA KUTUSU (Faz 8.2).
--
-- NEDEN VAR: e-posta bu projede bilinçli olarak MOCK (docs/0.0-constraints.md
-- §2) ve mock adapter gönderimi loga yazıyor. Bu, 6.4'ün çift onay akışını
-- denemenin TEK yoluydu ve o yol `docker compose logs` okumaktan geçiyordu —
-- yani akışın çalıştığını göstermek için terminal açmak gerekiyordu. Bu tablo
-- aynı gönderimi bir de SAKLIYOR, böylece onay/davet/parola sıfırlama linkleri
-- panodaki bir ekrandan tıklanabiliyor.
--
-- LOG'UN YERİNE DEĞİL, YANINA: mock adapter loga yazmaya devam ediyor. Log
-- satırı 6.3'ten beri gerçek gönderimin alanlarını taşıyor ve bu tablonun
-- yazılamadığı durumda geriye kalan tek kayıt o (bkz. adapters.go).
--
-- ORG_ID YOK ve bu bilinçli: buradaki mesajların hepsi org'a ait değil.
-- Parola sıfırlama (8.4) bir organizasyondan önce geliyor ve abonelik onayı
-- (6.4) bir ziyaretçiye ait. Nullable bir `org_id`, satırların yarısında NULL
-- duran ve bir gün yanlışlıkla süzgeç olarak kullanılacak bir sütun olurdu.
-- İzolasyonun yerine ORTAM KAPISI konuyor: bu tabloyu okuyan uç yalnızca
-- APP_ENV AÇIKÇA development|test iken router'a kaydediliyor (7.5'in
-- envExplicit disiplini).
CREATE TABLE email_outbox (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Alıcı adresi. Kullanıcı tablosuna yabancı anahtar YOK: alıcı çoğu zaman
    -- bir `User` değil (abone bir kullanıcı değil — 0.1; davet edilen kişi
    -- henüz kaydolmamış olabilir — 8.3).
    recipient  text NOT NULL,

    -- Konu satırını ADAPTER üretiyor (6.3'ün kararı: "subject" bir e-posta
    -- kavramı, webhook'un öyle bir alanı yok) — bu yüzden burada saklanan şey
    -- gerçekten gönderilmiş olan metin, sonradan yeniden türetilmiş değil.
    subject    text NOT NULL,
    body       text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Ekranın tek sorgusu: en yeniden eskiye. `id` ikincil anahtar olarak
-- eklendi — aynı milisaniyede yazılan iki mesajın sırası kararlı kalsın
-- (bir davet e-postası ile onun tetiklediği bildirim aynı ana düşebilir).
CREATE INDEX email_outbox_created_at_idx ON email_outbox (created_at DESC, id DESC);

-- BUDAMA JOB'I YOK ve gerekçesi yazılı: bu tablo yalnızca geliştirme/test
-- ortamında yazılıyor (üretimde outbox hiç bağlanmıyor, bkz. NewRegistry) ve
-- oradaki hacim mertebe olarak "birkaç yüz satır". 3.4'ün budama job'ının
-- karşılığını buraya kurmak, kullanılmayan altyapı taşımak olurdu (0.2).
-- Okuma ucu yine de sınırlı sayıda satır döndürüyor.

-- +goose Down
DROP TABLE IF EXISTS email_outbox;
