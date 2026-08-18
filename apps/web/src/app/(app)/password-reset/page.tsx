'use client';

import Link from 'next/link';
import { useState, type FormEvent } from 'react';

import { ApiError, request } from '@/lib/api';
import styles from './page.module.css';

/**
 * PAROLA SIFIRLAMA İSTEĞİ (8.4).
 *
 * ARAYÜZ SUNUCUNUN SESSİZLİĞİNİ BOZMUYOR: sunucu adresin kayıtlı olup
 * olmadığını bilinçli olarak söylemiyor (ne gövdede ne SÜREDE), bu ekran da
 * "böyle bir kullanıcı yok" gibi bir ayrım UYDURMUYOR. Gönderildikten sonra
 * her durumda aynı metin çıkıyor.
 *
 * OTURUMSUZ: `useSession` KULLANILMIYOR ve istek doğrudan `request` ile
 * atılıyor — parolasını unutan kişinin tanımı gereği oturumu yok.
 */
export default function PasswordResetPage() {
  const [email, setEmail] = useState('');
  const [sent, setSent] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);

    try {
      await request('/v1/auth/password-reset', {
        method: 'POST',
        body: { email },
      });
      setSent(true);
    } catch (cause) {
      // Buraya yalnızca AĞ/hız sınırı hataları düşüyor: sunucu adres için
      // hiçbir koşulda hata döndürmüyor.
      setError(
        cause instanceof ApiError ? cause.message : 'İstek gönderilemedi',
      );
    } finally {
      setSubmitting(false);
    }
  }

  if (sent) {
    return (
      <main className={styles.page}>
        <div className={styles.card}>
          <h1 className={styles.title}>Bağlantı gönderildi</h1>
          <p className={styles.body}>
            Adres kayıtlıysa parola sıfırlama bağlantısı gönderildi. Bağlantı
            bir saat geçerli ve yalnızca bir kez kullanılabilir.
          </p>
          <Link className={styles.link} href="/login">
            Giriş ekranına dön
          </Link>
        </div>
      </main>
    );
  }

  return (
    <main className={styles.page}>
      <form className={styles.card} onSubmit={handleSubmit} noValidate>
        <h1 className={styles.title}>Parolamı unuttum</h1>
        <p className={styles.body}>
          Hesabınızın e-posta adresini yazın; sıfırlama bağlantısı göndereceğiz.
        </p>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        <label className={styles.field}>
          <span className={styles.label}>E-posta</span>
          <input
            className={styles.input}
            type="email"
            name="email"
            autoComplete="email"
            required
            value={email}
            onChange={(event) => setEmail(event.target.value)}
          />
        </label>

        <button className={styles.submit} type="submit" disabled={submitting}>
          {submitting ? 'Gönderiliyor…' : 'Sıfırlama bağlantısı gönder'}
        </button>

        <Link className={styles.link} href="/login">
          Giriş ekranına dön
        </Link>
      </form>
    </main>
  );
}
