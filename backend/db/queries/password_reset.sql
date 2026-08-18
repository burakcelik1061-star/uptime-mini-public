-- Parola sıfırlama (Faz 8.4).

-- name: CreatePasswordResetToken :execrows
-- Sıfırlama token'ını YAZAR — kullanıcı varsa.
--
-- `INSERT … SELECT` BİLİNÇLİ, "önce kullanıcıyı bul, sonra yaz" DEĞİL:
-- adresin kayıtlı olup olmadığı çağırana sızmamalı (1.1'in giriş ucundaki
-- karar) ve iki ayrı sorgu, iki farklı sayıda gidiş-geliş demekti. Tek
-- ifade her iki durumda da AYNI işi yapıyor; fark yalnızca yazılan satır
-- sayısında (0 veya 1) ve o sayı çağıranın cevabına yansımıyor.
INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
SELECT u.id, sqlc.arg(token_hash), sqlc.arg(expires_at)
FROM users u
WHERE u.email = sqlc.arg(email);

-- name: GetPasswordResetToken :one
-- Token'ın sahibini ve süresini döner.
--
-- SÜRE BURADA SÜZÜLMÜYOR, çağıran ayırıyor: "süresi dolmuş" ile "hiç yok"
-- farkı kullanıcıya anlamlı bir mesaj vermek için gerekli (yenisini iste vs.
-- linkin yanlış) ve token zaten tahmin edilemez olduğu için bu ayrım bir
-- sızıntı değil (8.3'ün davet kabulüyle aynı gerekçe).
SELECT t.id, t.user_id, t.expires_at, u.email
FROM password_reset_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = sqlc.arg(token_hash);

-- name: DeletePasswordResetTokensForUser :execrows
-- Kullanıcının TÜM sıfırlama token'larını siler.
--
-- Yalnızca kullanılanı değil HEPSİNİ: saldırgan da bir sıfırlama isteyip
-- token'ını beklemede tutabilir. Meşru kullanıcı parolasını değiştirdiğinde
-- o token da ölmeli.
DELETE FROM password_reset_tokens
WHERE user_id = sqlc.arg(user_id);

-- name: UpdateUserPassword :execrows
UPDATE users
SET password_hash = sqlc.arg(password_hash)
WHERE id = sqlc.arg(id);

-- name: RevokeAllUserSessions :execrows
-- Kullanıcının TÜM refresh oturumlarını iptal eder (8.4).
--
-- 1.2'nin `RevokeSession`ı TEK bir aileyi (session_id) kapatıyor — logout ve
-- reuse tespiti için doğru olan o. Parola sıfırlama farklı bir soru soruyor:
-- "bu hesaba erişimi olan HERKES düşsün". Parolasını sıfırlayan kişinin
-- gerekçesi çoğu zaman hesabının ele geçirilmiş olması ve saldırganın açık
-- oturumu, parola değişse bile 30 gün daha yaşardı (refresh TTL).
UPDATE refresh_tokens
SET revoked_at = now()
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;
