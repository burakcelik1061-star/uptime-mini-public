-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (session_id, user_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING id, session_id, user_id, token_hash, issued_at, expires_at, used_at, revoked_at;

-- name: LockRefreshTokenByHash :one
-- FOR UPDATE bilinçli: aynı token'la eş zamanlı gelen iki yenileme isteği
-- bu satırda sıraya giriyor. İkincisi kilidi aldığında `used_at` dolmuş
-- oluyor ve "yeniden kullanım" olarak işleniyor — yoksa ikisi de geçerli
-- sayılıp aynı aileden iki paralel zincir doğardı.
SELECT id, session_id, user_id, token_hash, issued_at, expires_at, used_at, revoked_at
FROM refresh_tokens
WHERE token_hash = $1
FOR UPDATE;

-- name: MarkRefreshTokenUsed :exec
UPDATE refresh_tokens
SET used_at = now()
WHERE id = $1;

-- name: RevokeSession :execrows
-- Oturum ailesinin tamamını iptal eder (logout ve reuse tespiti).
UPDATE refresh_tokens
SET revoked_at = now()
WHERE session_id = $1
  AND revoked_at IS NULL;
